// ruview-proxy puts RuView's dashboard behind one local port so it can be
// published through a single Cloudflare quick tunnel (see ../../ruview.sh
// --public).
//
// Why this exists at all: RuView's sensing-server serves its UI + REST API
// on one port and the live sensing WebSocket on a *second* one, and its UI
// decides which WebSocket port to dial from the port the page was loaded
// on (ui/services/sensing.service.js: 3000 -> 3001, 8080 -> 8765, anything
// else -> the same host:port as the page). A quick tunnel
// (`cloudflared tunnel --url ...`) forwards exactly one origin, and behind
// it the page's port is 443 - so the browser dials wss://<tunnel>/ws/sensing
// on the very same origin, which sensing-server's HTTP port never serves.
// This proxy is that one origin: /ws/* goes to the WebSocket listener,
// everything else to the HTTP one.
//
// The same problem exists locally: RuView's Observatory page
// (ui/observatory/js/main.js _autoDetectLive) finds the server via
// /health on its own origin and then dials ws://<that origin>/ws/sensing,
// without the 3000 -> 3001 mapping the main dashboard has - so on
// http://localhost:3000 it silently fell back to its demo generator
// (confirmed live). -local serves the dashboard on this machine's own
// port through the proxy too, keyless, so every page finds the stream on
// the origin it was loaded from.
//
// It also rewrites the Host header to the upstream's own address:
// sensing-server validates Host against a loopback-only allowlist (its
// host_validation.rs, a DNS-rebinding defence) and would reject
// "<random>.trycloudflare.com" outright.
//
// Optional access key (-key): the quick-tunnel hostname is random but
// public, and RuView's API runs unauthenticated by default, so the link
// ruview.sh sends you carries ?k=<key>; the first request with the right
// key sets a cookie and every request after that (including the WebSocket
// upgrade, which browsers send cookies on) is checked against it. Wrong or
// missing key -> 403, nothing proxied. This is not a substitute for real
// auth, it just means the link itself is the credential, the same model
// the ntfy topic already uses elsewhere in this project.
package main

import (
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

const cookieName = "ruview_k"

func upstream(addr string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	p := httputil.NewSingleHostReverseProxy(target)
	director := p.Director
	p.Director = func(req *http.Request) {
		director(req)
		req.Host = target.Host // see package doc: sensing-server's Host allowlist
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream %s %s: %v", addr, r.URL.Path, err)
		http.Error(w, "RuView backend not reachable: "+err.Error(), http.StatusBadGateway)
	}
	return p
}

func main() {
	listen := flag.String("listen", "127.0.0.1:3080", "address to serve on")
	httpAddr := flag.String("http", "127.0.0.1:3000", "sensing-server HTTP (UI + API) address")
	wsAddr := flag.String("ws", "127.0.0.1:3001", "sensing-server WebSocket (--ws-port) address")
	key := flag.String("key", "", "access key; when set, requests need ?k=<key> once (then a cookie)")
	local := flag.String("local", "", "optional second address serving the same thing WITHOUT the access key, for this machine's own browser (e.g. 127.0.0.1:3000) - see package doc")
	flag.Parse()

	httpProxy := upstream(*httpAddr)
	wsProxy := upstream(*wsAddr)

	route := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.Redirect(w, r, "/ui/index.html", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/ws/"):
			wsProxy.ServeHTTP(w, r)
		default:
			httpProxy.ServeHTTP(w, r)
		}
	}

	authorized := func(r *http.Request) bool {
		if *key == "" {
			return true
		}
		if c, err := r.Cookie(cookieName); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(*key)) == 1 {
			return true
		}
		return false
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *key != "" {
			if k := r.URL.Query().Get("k"); k != "" {
				if subtle.ConstantTimeCompare([]byte(k), []byte(*key)) != 1 {
					http.Error(w, "wrong access key", http.StatusForbidden)
					return
				}
				// Right key in the URL: remember it in a cookie and drop it
				// from the address bar so it isn't copied around further
				// than it has to be (the link itself is still the secret,
				// but it doesn't need to stay visible on screen).
				http.SetCookie(w, &http.Cookie{
					Name:     cookieName,
					Value:    *key,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					Secure:   strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
				})
				q := r.URL.Query()
				q.Del("k")
				clean := *r.URL
				clean.RawQuery = q.Encode()
				http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
				return
			}
			if !authorized(r) {
				http.Error(w, "this RuView dashboard needs the access link you were sent", http.StatusForbidden)
				return
			}
		}
		route(w, r)
	})

	if *local != "" {
		go func() {
			fmt.Fprintf(os.Stderr, "ruview-proxy: %s (local, no key) -> http %s, ws %s\n", *local, *httpAddr, *wsAddr)
			if err := http.ListenAndServe(*local, http.HandlerFunc(route)); err != nil {
				log.Fatal(err)
			}
		}()
	}
	if *listen == "" {
		select {} // local-only mode
	}

	fmt.Fprintf(os.Stderr, "ruview-proxy: %s -> http %s, ws %s (access key: %s)\n",
		*listen, *httpAddr, *wsAddr, map[bool]string{true: "on", false: "off"}[*key != ""])
	if err := http.ListenAndServe(*listen, handler); err != nil {
		log.Fatal(err)
	}
}
