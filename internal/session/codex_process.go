package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jaekwon-park/tgcc/internal/store"
)

func codexProcessIdentity(pid int64) (string, string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", "", fmt.Errorf("incomplete process stat")
	}
	return fields[19], fields[0], nil
}

func stopOrphanCodex(sess *store.Session) error {
	if sess.PID == 0 {
		return nil
	}
	started, state, err := codexProcessIdentity(sess.PID)
	if os.IsNotExist(err) || state == "Z" {
		return nil
	}
	if err != nil {
		return err
	}
	if sess.PID <= 1 || sess.CodexProcessStart == "" || started != sess.CodexProcessStart {
		return fmt.Errorf("Codex process identity changed for session %s; inspect PID %d before resuming", sess.ID, sess.PID)
	}
	data, err := os.ReadFile("/proc/" + strconv.FormatInt(sess.PID, 10) + "/environ")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	marker := []byte("TGCC_CODEX_SESSION_ID=" + sess.ID)
	matched := false
	for _, entry := range bytes.Split(data, []byte{0}) {
		if bytes.Equal(entry, marker) {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("Codex session marker missing for PID %d", sess.PID)
	}
	group, err := syscall.Getpgid(int(sess.PID))
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil || group != int(sess.PID) {
		return fmt.Errorf("Codex process group mismatch for PID %d", sess.PID)
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, state, err := codexProcessIdentity(sess.PID)
		if os.IsNotExist(err) || state == "Z" || (err == nil && current != started) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("Codex process %d did not stop", sess.PID)
}
