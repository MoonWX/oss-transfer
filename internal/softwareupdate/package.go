package softwareupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type packageIdentity struct {
	version, commit string
	files           map[string]asset
}

// Validate every archive member without extracting paths or loading binaries
// into memory. Only the native installer's fixed filenames are accepted.
func validateArchive(path, version, target string) (packageIdentity, error) {
	identity := packageIdentity{files: map[string]asset{}}
	root := "oss-transfer-" + strings.ReplaceAll(target, "/", "-")
	ext := ""
	if strings.HasPrefix(target, "windows/") {
		ext = ".exe"
	}
	allowed := map[string]bool{"VERSION": true, "SHA256SUMS": true, "README.md": true, "README.zh-CN.md": true}
	for _, role := range []string{"master", "agent", "cli"} {
		allowed["oss-transfer-"+role+ext] = true
	}
	seen := map[string]bool{}
	metadata := map[string]string{}
	var total int64
	consume := func(name string, size int64, directory bool, stream io.Reader) error {
		if seen[name] {
			return errors.New("Duplicate release archive member")
		}
		seen[name] = true
		if directory {
			if name != root+"/" {
				return errors.New("Unexpected archive directory")
			}
			return nil
		}
		base, ok := strings.CutPrefix(name, root+"/")
		if !ok || !allowed[base] || size < 1 || size > 512<<20 {
			return errors.New("Unexpected or oversized archive member")
		}
		total += size
		if total > archiveLimit {
			return errors.New("Expanded release archive is too large")
		}
		hash := sha256.New()
		var n int64
		var err error
		if base == "VERSION" || base == "SHA256SUMS" {
			if size > 4096 {
				return errors.New("Archive metadata is too large")
			}
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(stream, size+1))
			n = int64(len(raw))
			hash.Write(raw)
			metadata[base] = string(raw)
		} else {
			n, err = io.Copy(hash, io.LimitReader(stream, size+1))
		}
		if err != nil {
			return err
		}
		if n != size {
			return errors.New("Archive member size differs")
		}
		identity.files[base] = asset{Name: base, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
		return nil
	}
	if strings.HasSuffix(path, ".zip") {
		archive, err := zip.OpenReader(path)
		if err != nil {
			return identity, err
		}
		defer archive.Close()
		for _, member := range archive.File {
			if member.Mode()&os.ModeSymlink != 0 || (!member.Mode().IsRegular() && !member.FileInfo().IsDir()) || member.UncompressedSize64 > uint64(archiveLimit) {
				return identity, errors.New("Archive contains a link or non-regular file")
			}
			stream, err := member.Open()
			if err != nil {
				return identity, err
			}
			err = consume(member.Name, int64(member.UncompressedSize64), member.FileInfo().IsDir(), stream)
			err = errors.Join(err, stream.Close())
			if err != nil {
				return identity, err
			}
		}
	} else {
		file, err := os.Open(path)
		if err != nil {
			return identity, err
		}
		defer file.Close()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return identity, err
		}
		defer compressed.Close()
		archive := tar.NewReader(compressed)
		for {
			member, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return identity, err
			}
			if member.Typeflag != tar.TypeReg && member.Typeflag != tar.TypeRegA && member.Typeflag != tar.TypeDir {
				return identity, errors.New("Archive contains a link or non-regular file")
			}
			if err = consume(member.Name, member.Size, member.Typeflag == tar.TypeDir, archive); err != nil {
				return identity, err
			}
		}
		padding, err := io.ReadAll(io.LimitReader(compressed, 65537))
		if err != nil {
			return identity, err
		}
		if len(padding) > 65536 || strings.Trim(string(padding), "\x00") != "" {
			return identity, errors.New("Unexpected data after release archive")
		}
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(metadata["VERSION"], "\n"), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if _, duplicate := fields[key]; !ok || duplicate {
			return identity, errors.New("Invalid archive VERSION")
		}
		fields[key] = value
	}
	if fields["Version"] != version || fields["Platform"] != target || !commitPattern.MatchString(fields["FullCommit"]) {
		return identity, errors.New("Archive identity differs from the requested release")
	}
	checks := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(metadata["SHA256SUMS"]), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return identity, errors.New("Invalid archive checksum manifest")
		}
		name := strings.TrimPrefix(parts[1], "*")
		if checks[name] || (name != "VERSION" && name != "oss-transfer-master"+ext && name != "oss-transfer-agent"+ext && name != "oss-transfer-cli"+ext) {
			return identity, errors.New("Unexpected archive checksum entry")
		}
		if identity.files[name].SHA256 != parts[0] {
			return identity, errors.New("Archive content checksum mismatch")
		}
		checks[name] = true
	}
	if len(checks) != 4 {
		return identity, errors.New("Incomplete archive checksum manifest")
	}
	identity.version, identity.commit = version, fields["FullCommit"]
	return identity, nil
}

func (c *client) prepare(ctx context.Context, work, version, target string) (string, string, error) {
	var errs []error
	for index, source := range c.sourceOrder(ctx) {
		stage := filepath.Join(work, fmt.Sprintf("package-%d", index))
		if err := privateDirectory(stage); err != nil {
			return "", "", err
		}
		commit, err := c.prepareFrom(ctx, stage, version, target, source)
		if err == nil {
			return stage, commit, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", source, err))
		_ = os.RemoveAll(stage)
	}
	return "", "", errors.Join(errs...)
}
func (c *client) prepareFrom(ctx context.Context, stage, version, target, source string) (string, error) {
	release, err := c.releaseFrom(ctx, source, version)
	if err != nil {
		return "", err
	}
	name := "oss-transfer-" + strings.ReplaceAll(target, "/", "-")
	ext, installer, sourcePath := ".tar.gz", "native-install.sh", "deploy/native/install.sh"
	if strings.HasPrefix(target, "windows/") {
		ext, installer, sourcePath = ".zip", "Install.ps1", "deploy/native/Install.ps1"
	}
	archive, err := release.asset(name + ext)
	if err != nil {
		return "", err
	}
	archivePath := filepath.Join(stage, archive.Name)
	if err = c.download(ctx, archive, archivePath); err != nil {
		return "", err
	}
	identity, err := validateArchive(archivePath, version, target)
	if err != nil {
		return "", err
	}
	if release.Commit != "" && release.Commit != identity.commit {
		return "", errors.New("Release commit differs from archive")
	}
	installerPath := filepath.Join(stage, installer)
	var installerAsset asset
	if source == "jdoss" {
		installerAsset, err = release.asset(installer)
		if err != nil {
			return "", err
		}
		if installerAsset.Size > 1<<20 {
			return "", errors.New("Installer is too large")
		}
		err = c.download(ctx, installerAsset, installerPath)
	} else {
		file, createErr := os.OpenFile(installerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			return "", createErr
		}
		hash := sha256.New()
		var n int64
		n, err = c.fetch(ctx, "https://raw.githubusercontent.com/MoonWX/oss-transfer/"+identity.commit+"/"+sourcePath, 1<<20, io.MultiWriter(file, hash))
		err = errors.Join(err, file.Close())
		installerAsset = asset{Name: installer, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	}
	if err != nil {
		return "", err
	}
	if installerAsset.Size == 0 {
		return "", errors.New("Empty native installer")
	}
	parts := strings.Split(target, "/")
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "format\toss-transfer-go-v1\nversion\t%s\ncommit\t%s\n", version, identity.commit)
	for _, role := range []string{"master", "agent", "cli"} {
		suffix := ""
		if parts[0] == "windows" {
			suffix = ".exe"
		}
		file := identity.files["oss-transfer-"+role+suffix]
		fmt.Fprintf(&manifest, "file\t%s\t%s\t%s\toss-transfer-%s-%s-%s%s\t%s\t%d\n", parts[0], parts[1], role, role, parts[0], parts[1], suffix, file.SHA256, file.Size)
	}
	fmt.Fprintf(&manifest, "archive\t%s\t%s\t%s\t%s\t%d\nasset\t%s\t%s\t%d\n", parts[0], parts[1], archive.Name, archive.SHA256, archive.Size, installer, installerAsset.SHA256, installerAsset.Size)
	if err = os.WriteFile(filepath.Join(stage, "manifest.tsv"), []byte(manifest.String()), 0600); err != nil {
		return "", err
	}
	return identity.commit, nil
}
