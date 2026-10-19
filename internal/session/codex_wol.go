package session

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// isSSHConnectFailure reports whether err looks like a failed SSH TCP connect
// (host powered off / unreachable) rather than a codex-level failure. The
// remote ssh stderr is folded into the error string by attemptCodex, so a
// "ssh: connect to host ... port 22: Connection timed out" message matches.
func isSSHConnectFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection timed out") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host")
}

// sendWOL sends a Wake-on-LAN magic packet for mac by SSHing into relayHost
// (which lives on the same L2 subnet as the sleeping host) and broadcasting the
// packet from there. It prefers the `wakeonlan` binary when available on the
// relay and falls back to a self-contained Python one-liner.
func (runner *codexRunner) sendWOL(ctx context.Context, relayHost, mac string) error {
	remote := buildWOLRemoteCommand(mac, wolBroadcast(relayHost))
	command := exec.CommandContext(ctx, runner.sshBin,
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", relayHost, remote)
	command.Env = mergedCodexEnv(runner.env)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("WOL wake via %s: %w: %s", relayHost, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildWOLRemoteCommand renders the shell command to run on the relay host.
func buildWOLRemoteCommand(mac, directedBroadcast string) string {
	return fmt.Sprintf("command -v wakeonlan >/dev/null 2>&1 && wakeonlan %s || python3 -c %s",
		shellQuote(mac), shellQuote(wolPythonScript(mac, directedBroadcast)))
}

// wolPythonScript builds a self-contained Python magic-packet sender. It emits
// no single quotes so the whole script can be shell-single-quoted verbatim.
// It broadcasts to 255.255.255.255 (limited broadcast, always reaches the local
// subnet) and, when directedBroadcast is non-empty, to the subnet's directed
// broadcast as well.
func wolPythonScript(mac, directedBroadcast string) string {
	dsts := `"255.255.255.255"`
	if directedBroadcast != "" {
		dsts += `, "` + directedBroadcast + `"`
	}
	return "import socket\n" +
		`mac="` + mac + `"` + "\n" +
		`mb=bytes.fromhex(mac.replace(":",""))` + "\n" +
		`p=b"\xff"*6+mb*16` + "\n" +
		`dsts=[` + dsts + `]` + "\n" +
		"for d in dsts:\n" +
		"    for port in (9,7):\n" +
		"        s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)\n" +
		"        s.setsockopt(socket.SOL_SOCKET,socket.SO_BROADCAST,1)\n" +
		"        s.sendto(p,(d,port))\n" +
		"        s.close()\n"
}

// wolBroadcast derives a /24 directed broadcast address from the relay host
// string (e.g. "insainty@10.1.1.21" → "10.1.1.255"). It returns "" when no
// IPv4 literal can be extracted (e.g. the relay is given as a bare hostname),
// in which case only the limited broadcast is used.
func wolBroadcast(relayHost string) string {
	host := relayHost
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	ip4[3] = 255
	return ip4.String()
}

// shellQuote single-quotes s for a POSIX shell (the relay host's login shell).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
