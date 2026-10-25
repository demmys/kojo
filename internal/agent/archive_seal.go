package agent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/loppo-llc/kojo/internal/configdir"
)

// Sealed archives.
//
// Archiving an agent packs its data directory (agentDir(id)) into a
// gzip-compressed tar, encrypts it with age's scrypt (passphrase)
// recipient, writes it to <configdir>/archives/agents/<id>.tar.gz.age
// and removes the plaintext directory. Unarchive needs the same
// passcode to decrypt and restore the tree.
//
// The archive file's existence IS the "sealed" state: every disk<->DB
// sync path checks AgentSealed and stays out of agentDir while it is
// present, so nothing re-materializes plaintext (persona.md, MEMORY.md,
// memory/) next to the encrypted copy.
//
// Scope: only the directory is sealed. DB-canonical rows (transcript,
// persona/memory rows, tasks, credentials) are untouched.

var (
	// ErrArchivePasscodeRequired is returned when archive / unarchive
	// of a sealed agent is attempted without a passcode.
	ErrArchivePasscodeRequired = errors.New("archive passcode required")
	// ErrArchivePasscodeInvalid is returned when the passcode does not
	// decrypt the sealed archive.
	ErrArchivePasscodeInvalid = errors.New("archive passcode is incorrect")
	// ErrAgentSealed is returned by operations that need the agent's
	// plaintext directory (fork, device switch) while it is sealed.
	ErrAgentSealed = errors.New("agent data is sealed in an encrypted archive")
)

// MinArchivePasscodeLen is the minimum passcode length (in runes).
const MinArchivePasscodeLen = 8

const sealedArchiveExt = ".tar.gz.age"

func sealedArchivesDir() string {
	return filepath.Join(configdir.Path(), "archives", "agents")
}

func sealedArchivePath(id string) string {
	return filepath.Join(sealedArchivesDir(), id+sealedArchiveExt)
}

// AgentSealed reports whether the agent's directory is currently held
// in an encrypted archive.
func AgentSealed(id string) bool {
	if id == "" {
		return false
	}
	_, err := os.Lstat(sealedArchivePath(id))
	return err == nil
}

func validateArchivePasscode(passcode string) error {
	if passcode == "" {
		return ErrArchivePasscodeRequired
	}
	if len([]rune(passcode)) < MinArchivePasscodeLen {
		return fmt.Errorf("%w: passcode must be at least %d characters", ErrArchivePasscodeRequired, MinArchivePasscodeLen)
	}
	return nil
}

func randSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sealAgentDir encrypts agentDir(id) into the sealed archive and
// removes the plaintext tree. Caller must have stopped all runtime
// activity and hold the per-agent memory/persona sync locks.
//
// The archive is fully written, fsynced and then re-read with the
// passcode before the plaintext is deleted, so a write or encryption
// fault can never cost the only copy.
func sealAgentDir(id, passcode string) error {
	if err := validateArchivePasscode(passcode); err != nil {
		return err
	}
	if AgentSealed(id) {
		return nil
	}
	dir := agentDir(id)
	if err := os.MkdirAll(sealedArchivesDir(), 0o700); err != nil {
		return fmt.Errorf("seal: ensure archive dir: %w", err)
	}
	final := sealedArchivePath(id)
	tmp := final + ".tmp-" + randSuffix()
	written, err := writeSealedArchive(tmp, dir, passcode)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	verified, err := countSealedArchive(tmp, passcode)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("seal: verify archive: %w", err)
	}
	if verified != written {
		_ = os.Remove(tmp)
		return fmt.Errorf("seal: verify archive: wrote %d entries, read back %d", written, verified)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("seal: install archive: %w", err)
	}
	_ = syncDir(sealedArchivesDir())
	if err := os.RemoveAll(dir); err != nil {
		// The archive is authoritative from here on; leftover
		// plaintext is overwritten by Unarchive's restore.
		return fmt.Errorf("seal: remove plaintext dir (archive is intact): %w", err)
	}
	return nil
}

// writeSealedArchive streams dir as tar.gz into an age-encrypted file
// at path. Regular files, directories and symlinks are kept; other
// file types (sockets, fifos, devices) are skipped. Returns the number
// of tar entries written.
func writeSealedArchive(path, dir, passcode string) (int, error) {
	rcp, err := age.NewScryptRecipient(passcode)
	if err != nil {
		return 0, fmt.Errorf("seal: recipient: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("seal: create archive: %w", err)
	}
	defer f.Close()
	enc, err := age.Encrypt(f, rcp)
	if err != nil {
		return 0, fmt.Errorf("seal: encrypt: %w", err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)

	count := 0
	if _, statErr := os.Lstat(dir); statErr == nil {
		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == dir {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := info.Mode()
			var link string
			switch {
			case mode.IsDir(), mode.IsRegular():
			case mode&fs.ModeSymlink != 0:
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			default:
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if mode.IsDir() {
				hdr.Name += "/"
			}
			hdr.Uname, hdr.Gname = "", ""
			hdr.Format = tar.FormatPAX
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			count++
			if mode.IsRegular() {
				src, err := os.Open(p)
				if err != nil {
					return err
				}
				n, err := io.Copy(tw, src)
				src.Close()
				if err != nil {
					return err
				}
				if n != hdr.Size {
					return fmt.Errorf("%s changed size while archiving", rel)
				}
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("seal: archive dir: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return 0, fmt.Errorf("seal: stat dir: %w", statErr)
	}
	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("seal: tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("seal: gzip close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return 0, fmt.Errorf("seal: encrypt close: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("seal: fsync: %w", err)
	}
	return count, f.Close()
}

// openSealedArchive opens and decrypts path, mapping a wrong passcode
// to ErrArchivePasscodeInvalid.
func openSealedArchive(path, passcode string) (*tar.Reader, func(), error) {
	id, err := age.NewScryptIdentity(passcode)
	if err != nil {
		return nil, nil, fmt.Errorf("unseal: identity: %w", err)
	}
	// age's default scrypt work-factor cap (22) is above what
	// NewScryptRecipient writes (18); keep the default.
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	dec, err := age.Decrypt(f, id)
	if err != nil {
		f.Close()
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, nil, ErrArchivePasscodeInvalid
		}
		return nil, nil, fmt.Errorf("unseal: decrypt: %w", err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("unseal: gzip: %w", err)
	}
	return tar.NewReader(gz), func() { gz.Close(); f.Close() }, nil
}

func countSealedArchive(path, passcode string) (int, error) {
	tr, closeFn, err := openSealedArchive(path, passcode)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	n := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return n, err
		}
		n++
	}
}

// unsealAgentDir decrypts the sealed archive into agentDir(id) and
// removes the archive. Extraction goes to a staging dir first (via
// os.Root, so entries cannot escape it), and only a fully decrypted
// and authenticated archive is installed. Files that appeared in
// agentDir(id) while sealed are kept unless the archive has the same
// top-level entry, in which case the archive wins.
//
// Caller holds the per-agent memory/persona sync locks.
func unsealAgentDir(id, passcode string) error {
	if !AgentSealed(id) {
		return nil
	}
	if passcode == "" {
		return ErrArchivePasscodeRequired
	}
	staging := filepath.Join(sealedArchivesDir(), ".unseal-"+id+"-"+randSuffix())
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return fmt.Errorf("unseal: staging: %w", err)
	}
	if err := extractSealedArchive(sealedArchivePath(id), staging, passcode); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("unseal: chmod: %w", err)
	}
	dir := agentDir(id)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("unseal: ensure agents dir: %w", err)
		}
		if err := os.Rename(staging, dir); err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("unseal: install: %w", err)
		}
	} else {
		entries, err := os.ReadDir(staging)
		if err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("unseal: read staging: %w", err)
		}
		for _, e := range entries {
			dst := filepath.Join(dir, e.Name())
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("unseal: replace %s: %w", e.Name(), err)
			}
			if err := os.Rename(filepath.Join(staging, e.Name()), dst); err != nil {
				return fmt.Errorf("unseal: install %s: %w", e.Name(), err)
			}
		}
		_ = os.RemoveAll(staging)
	}
	if err := os.Remove(sealedArchivePath(id)); err != nil {
		return fmt.Errorf("unseal: remove archive: %w", err)
	}
	_ = syncDir(sealedArchivesDir())
	return nil
}

func extractSealedArchive(path, staging, passcode string) error {
	tr, closeFn, err := openSealedArchive(path, passcode)
	if err != nil {
		return err
	}
	defer closeFn()
	root, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("unseal: open staging: %w", err)
	}
	defer root.Close()
	type dirTime struct {
		name string
		t    time.Time
	}
	var dirTimes []dirTime
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("unseal: read archive: %w", err)
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if name == "" || !filepath.IsLocal(name) {
			return fmt.Errorf("unseal: unsafe entry %q", hdr.Name)
		}
		if parent := filepath.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("unseal: mkdir %s: %w", parent, err)
			}
		}
		perm := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, perm|0o700); err != nil {
				return fmt.Errorf("unseal: mkdir %s: %w", name, err)
			}
			_ = root.Chmod(name, perm|0o700)
			dirTimes = append(dirTimes, dirTime{name, hdr.ModTime})
		case tar.TypeReg:
			out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o600)
			if err != nil {
				return fmt.Errorf("unseal: create %s: %w", name, err)
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("unseal: write %s: %w", name, err)
			}
			_ = root.Chmod(name, perm)
			_ = root.Chtimes(name, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("unseal: symlink %s: %w", name, err)
			}
		default:
			// Not produced by writeSealedArchive; ignore.
		}
	}
	for i := len(dirTimes) - 1; i >= 0; i-- {
		_ = root.Chtimes(dirTimes[i].name, dirTimes[i].t, dirTimes[i].t)
	}
	return nil
}

// removeSealedArchive drops the sealed archive (agent deletion).
func removeSealedArchive(id string) error {
	if err := os.Remove(sealedArchivePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
