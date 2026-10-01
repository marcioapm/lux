package shim

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// prepareInputs makes $LUX_INPUTS, the workload user's, so the variable
// names a directory from the start.
func (s *Shim) prepareInputs() error {
	if s.cfg.InputsDir == "" {
		return nil
	}
	_, err := s.inputsMkdir(s.cfg.InputsDir)
	return err
}

// writeInputs writes an input's images to $LUX_INPUTS/<request id>/ as
// <n>-<name> (n from 1), and returns them with Path set. The directories
// are 0700 and the files 0600, the workload user's. Everything is opened
// under the volume's root (InputsRoot): a link the workload put on its
// volume cannot lead the shim's writes out of it.
func (s *Shim) writeInputs(requestID string, list []spec.Attachment) ([]spec.Attachment, error) {
	if len(list) == 0 {
		return list, nil
	}
	if s.cfg.InputsDir == "" {
		return nil, errors.New("no $LUX_INPUTS")
	}
	dir := filepath.Join(s.cfg.InputsDir, safeComponent(cmp.Or(requestID, "input")))
	root, err := s.inputsMkdir(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, _ := filepath.Rel(s.cfg.InputsRoot, dir)
	out := make([]spec.Attachment, len(list))
	for i, a := range list {
		name := strconv.Itoa(i+1) + "-" + safeComponent(a.Name)
		p := filepath.Join(rel, name)
		// Replaced, never written through: what is there may be the
		// workload's link.
		if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, werr := f.Write(a.Decoded())
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}
		_ = root.Lchown(p, s.user.uid, s.user.gid)
		a.Path = filepath.Join(dir, name)
		out[i] = a
	}
	return out, nil
}

// inputsMkdir makes dir (under InputsRoot) and its missing parents, each
// 0700 and the workload user's, and returns the root it is under.
func (s *Shim) inputsMkdir(dir string) (*os.Root, error) {
	rel, err := filepath.Rel(s.cfg.InputsRoot, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("%s is not under %s", dir, s.cfg.InputsRoot)
	}
	root, err := os.OpenRoot(s.cfg.InputsRoot)
	if err != nil {
		return nil, err
	}
	at := ""
	for _, part := range strings.Split(rel, "/") {
		at = filepath.Join(at, part)
		err := root.Mkdir(at, 0o700)
		switch {
		case err == nil:
			_ = root.Lchown(at, s.user.uid, s.user.gid)
		case !errors.Is(err, fs.ErrExist):
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

// safeComponent is s as one file name: bytes other than letters, digits,
// '.', '_' and '-' become '_', at most 100 bytes; a name it changed (or
// "." or "..") gets 8 hex digits of s's sha256, so two names stay two.
func safeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 100 {
		out = out[:100]
	}
	if out == s && s != "." && s != ".." {
		return out
	}
	sum := sha256.Sum256([]byte(s))
	return out + "-" + hex.EncodeToString(sum[:4])
}

// rememberAttachments keeps an input's attachment metadata for its
// records (the adapter may report the input without its payload).
func (s *Shim) rememberAttachments(in proto.Input) {
	if in.RequestID == "" || len(in.Attachments) == 0 {
		return
	}
	s.mu.Lock()
	if s.inputMeta == nil {
		s.inputMeta = map[string][]spec.AttachmentMeta{}
	}
	s.inputMeta[in.RequestID] = spec.AttachmentsMeta(in.Attachments)
	s.mu.Unlock()
}

// attachmentsOf is the metadata kept for a request id; with forget, it is
// dropped (no record of the input follows).
func (s *Shim) attachmentsOf(id string, forget bool) []spec.AttachmentMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.inputMeta[id]
	if forget {
		delete(s.inputMeta, id)
	}
	return m
}
