// Package app wires configuration, storage, HTTP, and process lifecycle.
package app

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Source struct {
	Name   string `yaml:"name"`
	Root   string `yaml:"root"`
	Marker string `yaml:"marker"`
}

type Config struct {
	Listen    string   `yaml:"listen"`
	BaseURL   string   `yaml:"base_url"`
	Database  string   `yaml:"database"`
	CacheRoot string   `yaml:"cache_root"`
	WebRoot   string   `yaml:"web_root"`
	Sources   []Source `yaml:"sources"`
}

// LoadConfig resolves relative paths against the config file, not the invocation
// directory. Missing explicit configuration is an error, never a silent fallback.
func LoadConfig(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", BaseURL: "https://localhost:8443", Database: "data/gallerygo.db", CacheRoot: "cache", WebRoot: "web/dist"}
	abs, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return c, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return c, errors.New("configuration must be a regular file no larger than 1 MiB")
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	return c, c.validate(filepath.Dir(abs))
}

func (c *Config) validate(base string) error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be a host:port address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("listen port must be between 1 and 65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return errors.New("listen host must be an IP address or localhost")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("base_url must be an HTTPS origin without credentials, path, query, or fragment")
	}
	if u.Port() != "" {
		if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
			return errors.New("base_url port must be between 1 and 65535")
		}
	}
	c.BaseURL = strings.TrimSuffix(c.BaseURL, "/")
	for _, path := range []*string{&c.Database, &c.CacheRoot, &c.WebRoot} {
		*path, err = canonicalPath(base, *path)
		if err != nil {
			return err
		}
	}
	paths := []string{filepath.Dir(c.Database), c.CacheRoot, c.WebRoot}
	names := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" || names[s.Name] {
			return errors.New("source names must be nonempty and unique")
		}
		names[s.Name] = true
		if !filepath.IsLocal(s.Marker) || s.Marker == "." {
			return errors.New("each source requires a root-relative mount marker")
		}
		s.Root, err = canonicalPath(base, s.Root)
		if err != nil {
			return err
		}
		paths = append(paths, s.Root)
	}
	for i, p := range paths {
		for _, q := range paths[:i] {
			if overlaps(p, q) {
				return errors.New("database directory, cache, web assets, and source roots must not overlap")
			}
		}
	}
	return nil
}

func canonicalPath(base, path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) || strings.Contains(path, "://") {
		return "", errors.New("storage paths must be nonempty filesystem paths")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve storage path: %w", err)
	}
	parent, err := canonicalPath(base, filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func overlaps(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
