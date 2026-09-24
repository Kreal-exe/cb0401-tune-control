package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const appVersion = "0.2.0"

//go:embed templates/index.html
var templatesFS embed.FS

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func errResp(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) {
	_ = json.NewDecoder(r.Body).Decode(v)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	modem, err := getModemConfig()
	var modemOut any = modem
	if err != nil {
		modemOut = map[string]string{"_error": err.Error()}
	}
	wifi := getWifiStatus()
	ok(w, map[string]any{"modem": modemOut, "wifi": wifi})
}

func handleSA(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode *int `json:"mode"`
	}
	decodeBody(r, &body)
	if body.Mode == nil {
		errResp(w, fmt.Errorf("missing mode (0=SA+NSA, 1=SA off, 2=force SA, 3=5G off)"))
		return
	}
	cfg, err := setNr5gMode(*body.Mode)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleBands(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nr5gBand    string `json:"nr5g_band"`
		NsaNr5gBand string `json:"nsa_nr5g_band"`
		LteBand     string `json:"lte_band"`
	}
	decodeBody(r, &body)
	cfg, err := setBands(body.Nr5gBand, body.NsaNr5gBand, body.LteBand)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleWifi(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Band    string `json:"band"`
		Channel any    `json:"channel"`
		Bw      any    `json:"bw"`
	}
	decodeBody(r, &body)
	status, err := setWifi(body.Band, anyToStr(body.Channel), anyToStr(body.Bw))
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, status)
}

func anyToStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func handleUciDump(w http.ResponseWriter, r *http.Request) {
	out, err := rawShell("uci show 2>&1")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"output": out})
}

func handleSSHInfo(w http.ResponseWriter, r *http.Request) {
	// The router's old dropbear only offers ssh-rsa (SHA-1) as a host key -
	// a modern OpenSSH client (8.8+) rejects that by default ("no matching
	// host key type found"). It also regenerates that host key on every
	// reboot (ramfs /etc), so a cached known_hosts entry from before the
	// last reboot makes OpenSSH refuse password auth outright ("REMOTE HOST
	// IDENTIFICATION HAS CHANGED"). Without these flags the command may not
	// connect at all, so they always need to be in the hint shown to the user.
	sshCompat := "-o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no"
	ok(w, map[string]string{
		"host":     routerIP,
		"user":     "root",
		"key_path": keyPath,
		"cmd_key":  fmt.Sprintf("ssh %s -i %s root@%s", sshCompat, keyPath, routerIP),
		"cmd_pw":   fmt.Sprintf("ssh %s root@%s", sshCompat, routerIP),
	})
}

func handleReboot(w http.ResponseWriter, r *http.Request) {
	ok(w, rebootRouter())
}

func handleSpoofVersion(w http.ResponseWriter, r *http.Request) {
	reported, err := spoofFirmwareVersion()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"reported": reported})
}

func handleUnlock5GBands(w http.ResponseWriter, r *http.Request) {
	result, err := unlock5GBands()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"result": result})
}

func handleSetRootPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	decodeBody(r, &body)
	if err := setRootPassword(body.Password); err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]bool{"changed": true})
}

func handleWifiScan(w http.ResponseWriter, r *http.Request) {
	band := r.URL.Query().Get("band")
	if band == "" {
		band = "2.4"
	}
	result, err := scanWifiChannels(band)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, result)
}

func handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	result, err := getSystemHealth()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, result)
}

func handleDeviceMonitor(w http.ResponseWriter, r *http.Request) {
	state, err := getDeviceMonitorState()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, state)
}

func handleDeviceMonitorWhitelist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Macs []string `json:"macs"`
	}
	decodeBody(r, &body)
	state, err := setDeviceWhitelist(body.Macs)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, state)
}

func handleNotifyConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Backend          string  `json:"backend"`
		TelegramBotToken *string `json:"telegram_bot_token"`
		TelegramChatID   *string `json:"telegram_chat_id"`
		SmsForward       *bool   `json:"sms_forward"`
	}
	decodeBody(r, &body)
	cfg, err := setNotifyConfig(body.Backend, body.TelegramBotToken, body.TelegramChatID, body.SmsForward)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleRaw(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cmd string `json:"cmd"`
	}
	decodeBody(r, &body)
	if body.Cmd == "" {
		errResp(w, fmt.Errorf("Empty command"))
		return
	}
	out, err := rawShell(body.Cmd)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"output": out})
}

func requireMethod(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func loadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key, val := line[:idx], line[idx+1:]
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// setEnvValue rewrites one KEY=... line in .env (adding it if missing),
// leaving every other line untouched. Used so a change made through the
// GUI (e.g. a new root password) is reflected on disk immediately, not
// just in this process's memory - otherwise the next GUI restart would
// silently fall back to the stale value.
func setEnvValue(key, value string) error {
	existing, err := os.ReadFile(envFilePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	found := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !found && strings.HasPrefix(trimmed, key+"=") {
			lines = append(lines, key+"="+value)
			found = true
			continue
		}
		lines = append(lines, line)
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(envFilePath, []byte(content), 0o600)
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exeDir := filepath.Dir(exe)
	envFilePath = filepath.Join(exeDir, ".env")
	loadEnvFile(envFilePath)
	keyPath = filepath.Join(exeDir, "router_key")
	// Re-read config that depends on .env, now that it's loaded.
	routerIP = getenv("ROUTER_IP", routerIP)
	routerPass = getenv("ROUTER_ROOT_PASSWORD", routerPass)
	ntfyTopicEnv = getenv("NTFY_TOPIC", ntfyTopicEnv)
	notifyBackendEnv = getenv("NOTIFY_BACKEND", notifyBackendEnv)
	telegramTokenEnv = getenv("TELEGRAM_BOT_TOKEN", telegramTokenEnv)
	telegramChatEnv = getenv("TELEGRAM_CHAT_ID", telegramChatEnv)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/sa", requireMethod(http.MethodPost, handleSA))
	mux.HandleFunc("/api/bands", requireMethod(http.MethodPost, handleBands))
	mux.HandleFunc("/api/wifi", requireMethod(http.MethodPost, handleWifi))
	mux.HandleFunc("/api/uci-dump", handleUciDump)
	mux.HandleFunc("/api/ssh-info", handleSSHInfo)
	mux.HandleFunc("/api/reboot", requireMethod(http.MethodPost, handleReboot))
	mux.HandleFunc("/api/spoof-version", requireMethod(http.MethodPost, handleSpoofVersion))
	mux.HandleFunc("/api/unlock-5g-bands", requireMethod(http.MethodPost, handleUnlock5GBands))
	mux.HandleFunc("/api/root-password", requireMethod(http.MethodPost, handleSetRootPassword))
	mux.HandleFunc("/api/wifi-scan", handleWifiScan)
	mux.HandleFunc("/api/system-health", handleSystemHealth)
	mux.HandleFunc("/api/device-monitor", handleDeviceMonitor)
	mux.HandleFunc("/api/device-monitor/whitelist", requireMethod(http.MethodPost, handleDeviceMonitorWhitelist))
	mux.HandleFunc("/api/notify-config", requireMethod(http.MethodPost, handleNotifyConfig))
	mux.HandleFunc("/api/raw", requireMethod(http.MethodPost, handleRaw))

	addr := "127.0.0.1:5757"
	fmt.Println("============================================================")
	fmt.Printf("XIAOMI 5G CPE PRO CB0401V1/V2 Tune + Control: http://%s\n", addr)
	fmt.Println("============================================================")
	log.Fatal(http.ListenAndServe(addr, mux))
}
