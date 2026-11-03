package testdata

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxEntry = 1 << 30 // files > 1 GiB are rejected (§9)

// Unzip extracts src into dst, rejecting absolute paths, "..", symlinks and oversized
// files. Files are written 0640, directories 0750.
func Unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := f.Name
		if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
			return fmt.Errorf("zip entry %q: absolute or non-portable path", name)
		}
		clean := path.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains("/"+clean+"/", "/../") {
			return fmt.Errorf("zip entry %q: path escapes package", name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || (mode&os.ModeType != 0 && !mode.IsDir()) {
			return fmt.Errorf("zip entry %q: symlinks and special files are not allowed", name)
		}
		target := filepath.Join(dst, filepath.FromSlash(clean))
		if mode.IsDir() || strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if f.UncompressedSize64 > maxEntry {
			return fmt.Errorf("zip entry %q: larger than 1 GiB", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := extractOne(f, target); err != nil {
			return fmt.Errorf("zip entry %q: %w", name, err)
		}
	}
	return nil
}

func extractOne(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	// Do not trust the header size: cap the actual stream too.
	n, err := io.Copy(out, io.LimitReader(rc, maxEntry+1))
	if err == nil && n > maxEntry {
		err = fmt.Errorf("larger than 1 GiB")
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
