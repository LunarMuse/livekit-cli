package agentfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/schollz/progressbar/v3"

	"github.com/livekit/protocol/logger"
)

var standardTarballExcludeFiles = []string{
	"Dockerfile",
	".dockerignore",
	".gitignore",
	".git",
	"node_modules",
	"*.env",
}

func UploadTarball(directory string, presignedUrl string, excludeFiles []string) error {
	excludeFiles = append(standardTarballExcludeFiles, excludeFiles...)

	dockerIgnore := filepath.Join(directory, ".dockerignore")
	if _, err := os.Stat(dockerIgnore); err == nil {
		content, err := os.ReadFile(dockerIgnore)
		if err != nil {
			return fmt.Errorf("failed to read .dockerignore: %w", err)
		}
		excludeFiles = append(excludeFiles, strings.Split(string(content), "\n")...)
	}

	for i, exclude := range excludeFiles {
		excludeFiles[i] = strings.TrimSpace(exclude)
	}

	var totalSize int64
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(directory, path)
		if err != nil {
			return nil
		}

		for _, exclude := range excludeFiles {
			if exclude == "" || strings.Contains(exclude, "Dockerfile") {
				continue
			}
			if info.IsDir() {
				if strings.HasPrefix(relPath, exclude+"/") || strings.HasPrefix(relPath, exclude) {
					return filepath.SkipDir
				}
			}
			matched, err := filepath.Match(exclude, relPath)
			if err != nil {
				return nil
			}
			if matched {
				return nil
			}
		}

		if !info.IsDir() && info.Mode().IsRegular() {
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate total size: %w", err)
	}

	tarProgress := progressbar.NewOptions64(
		totalSize,
		progressbar.OptionSetDescription("Compressing files"),
		progressbar.OptionSetWidth(30),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)

	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	err = filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(directory, path)
		if err != nil {
			return fmt.Errorf("failed to calculate relative path for %s: %w", path, err)
		}

		for _, exclude := range excludeFiles {
			if exclude == "" || strings.Contains(exclude, "Dockerfile") {
				continue
			}

			if info.IsDir() {
				if strings.HasPrefix(relPath, exclude+"/") || strings.HasPrefix(relPath, exclude) {
					logger.Debugw("excluding directory from tarball", "path", path)
					return filepath.SkipDir
				}
			}

			matched, err := filepath.Match(exclude, relPath)
			if err != nil {
				return nil
			}
			if matched {
				logger.Debugw("excluding file from tarball", "path", path)
				return nil
			}
		}

		// Handle symlinks and get the real FileInfo if it's a symlink
		if info.Mode()&os.ModeSymlink != 0 {
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("failed to evaluate symlink %s: %w", path, err)
			}
			info, err = os.Stat(realPath)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %w", realPath, err)
			}
		}

		// Handle directories
		if info.IsDir() {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("failed to create tar header for directory %s: %w", path, err)
			}
			header.Name = relPath + "/"
			if err := tarWriter.WriteHeader(header); err != nil {
				return fmt.Errorf("failed to write tar header for directory %s: %w", path, err)
			}
			return nil
		}

		// Handle regular files
		if !info.Mode().IsRegular() {
			// Skip non-regular files (devices, pipes, etc.)
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", path, err)
		}
		defer file.Close()

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for file %s: %w", path, err)
		}
		header.Name = relPath
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for file %s: %w", path, err)
		}

		reader := io.TeeReader(file, tarProgress)
		_, err = io.Copy(tarWriter, reader)
		if err != nil {
			return fmt.Errorf("failed to copy file content for %s: %w", path, err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk directory: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	uploadProgress := progressbar.NewOptions64(
		int64(buffer.Len()),
		progressbar.OptionSetDescription("Uploading"),
		progressbar.OptionSetWidth(30),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)

	req, err := http.NewRequest("PUT", presignedUrl, io.TeeReader(&buffer, uploadProgress))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.ContentLength = int64(buffer.Len())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload tarball: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to upload tarball: %d: %s", resp.StatusCode, body)
	}

	fmt.Println()
	return nil
}
