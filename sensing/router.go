package main

// SSH access to the router: pulling finished capture files and listing the
// connected Wi-Fi clients. Shells out to the system ssh like the rest of
// this project (the router's dropbear only speaks ssh-rsa).

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
)

type router struct {
	keyPath string
	host    string
}

func (r *router) ssh(cmd string) ([]byte, error) {
	c := exec.Command("ssh",
		"-i", r.keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "PubkeyAcceptedKeyTypes=+ssh-rsa",
		"-o", "HostKeyAlgorithms=+ssh-rsa",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		r.host, cmd)
	c.Stdin = nil
	return c.Output()
}

type dumpFile struct {
	name string
	data []byte
}

// takeCompleted fetches and deletes every finished dump file in one ssh
// round-trip, oldest first. The newest file of each radio is left alone:
// the capture daemon has a reader writing into it right now.
func (r *router) takeCompleted() ([]dumpFile, error) {
	out, err := r.ssh(`cd /tmp || exit 1
for rd in wifi0 wifi1; do ls cfr_dump_${rd}_*.bin 2>/dev/null | sort | sed '$d'; done > .sensing_take
if [ -s .sensing_take ]; then tar cf - $(cat .sensing_take) && rm -f $(cat .sensing_take); fi`)
	if err != nil {
		return nil, err
	}
	var files []dumpFile
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, fmt.Errorf("tar stream: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return files, fmt.Errorf("tar entry %s: %w", hdr.Name, err)
		}
		files = append(files, dumpFile{name: path.Base(hdr.Name), data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// radioOf returns "wifi0"/"wifi1" from a cfr_dump_<radio>_<time>.bin name.
func radioOf(name string) string {
	rest := strings.TrimPrefix(name, "cfr_dump_")
	if i := strings.Index(rest, "_"); i > 0 {
		return rest[:i]
	}
	return rest
}

// station is one Wi-Fi client as the router sees it.
type station struct {
	MAC      string  `json:"mac"`
	Name     string  `json:"name"`
	IP       string  `json:"ip"`
	VAP      string  `json:"vap"`
	GHz      float64 `json:"ghz"`
	RSSI     int     `json:"rssi"`
	PowerSav bool    `json:"power_save"`
	Captured bool    `json:"captured"` // periodic CFR capture armed for it right now
}

// stations lists associated clients on every VAP (wlanconfig - this
// firmware's iw reports none), with DHCP names and whether the capture
// daemon has armed them.
func (r *router) stations() ([]station, error) {
	out, err := r.ssh(`for v in wl0 wl1 wl13 wl5; do
  f=$(iwconfig $v 2>/dev/null | sed -n 's/.*Frequency:\([0-9.]*\).*/\1/p')
  wlanconfig $v list sta 2>/dev/null | awk -v v=$v -v f=${f:-0} 'NR>1 && /^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}[[:space:]]/ {print "STA", v, f, tolower($1), $6, $NF}'
done
awk '{print "LEASE", tolower($2), $3, $4}' /tmp/dhcp.leases 2>/dev/null
awk '{print "ARMED", tolower($2)}' /tmp/cfr_periodic_armed.txt 2>/dev/null
true`)
	if err != nil {
		return nil, err
	}
	names, ips, armed := map[string]string{}, map[string]string{}, map[string]bool{}
	var list []station
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 6 && f[0] == "STA":
			ghz, _ := strconv.ParseFloat(f[2], 64)
			rssi, _ := strconv.Atoi(f[4])
			list = append(list, station{MAC: f[3], VAP: f[1], GHz: ghz, RSSI: rssi, PowerSav: f[5] != "0"})
		case len(f) >= 4 && f[0] == "LEASE":
			ips[f[1]] = f[2]
			if f[3] != "*" {
				names[f[1]] = f[3]
			}
		case len(f) == 2 && f[0] == "ARMED":
			armed[f[1]] = true
		}
	}
	for i := range list {
		list[i].Name = names[list[i].MAC]
		list[i].IP = ips[list[i].MAC]
		list[i].Captured = armed[list[i].MAC]
	}
	sort.Slice(list, func(i, j int) bool { return list[i].MAC < list[j].MAC })
	return list, nil
}
