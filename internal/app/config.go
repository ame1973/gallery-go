// Package app wires configuration, storage, HTTP, and process lifecycle.
package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/scanner"

	"go.yaml.in/yaml/v3"
)

type Source struct {
	Name   string `yaml:"name"`
	Root   string `yaml:"root"`
	Marker string `yaml:"marker"`
}

type WorkerConfig struct {
	Embedded    bool `yaml:"embedded"`
	jobs.Policy `yaml:",inline"`
}

type Config struct {
	Listen     string                 `yaml:"listen"`
	BaseURL    string                 `yaml:"base_url"`
	Database   string                 `yaml:"database"`
	CacheRoot  string                 `yaml:"cache_root"`
	WebRoot    string                 `yaml:"web_root"`
	Sources    []Source               `yaml:"sources"`
	Auth       auth.Policy            `yaml:"auth"`
	Worker     WorkerConfig           `yaml:"worker"`
	Scanner    scanner.Policy         `yaml:"scanner"`
	Inspection media.InspectPolicy    `yaml:"inspection"`
	Scheduler  scanner.SchedulePolicy `yaml:"scheduler"`
}

// LoadConfig resolves relative paths against the config file, not the invocation
// directory. Missing explicit configuration is an error, never a silent fallback.
func LoadConfig(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", BaseURL: "https://localhost:8443", Database: "data/gallerygo.db", CacheRoot: "cache", WebRoot: "web/dist", Auth: auth.DefaultPolicy(), Worker: WorkerConfig{Embedded: true, Policy: jobs.DefaultPolicy()}, Scanner: scanner.DefaultPolicy(), Inspection: media.DefaultInspectPolicy(), Scheduler: scanner.DefaultSchedulePolicy()}
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
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, errors.New("configuration must be no larger than 1 MiB")
	}
	// Inspect the bounded syntax tree before decoding defaults: YAML nulls, aliases
	// and merges must not silently retain or overwrite safety policy fields.
	var document yaml.Node
	syntax := yaml.NewDecoder(bytes.NewReader(data))
	if err := syntax.Decode(&document); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	var extra yaml.Node
	if err := syntax.Decode(&extra); !errors.Is(err, io.EOF) {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	if err := strictConfigNode(&document, 0); err != nil {
		return c, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	return c, c.validate(filepath.Dir(abs))
}

func strictConfigNode(node *yaml.Node, depth int) error {
	if depth > 32 || node.Kind == yaml.AliasNode || node.Anchor != "" || node.Tag == "!!null" || node.Tag == "!!merge" {
		return errors.New("configuration forbids nulls, aliases, anchors, merges, and excessive nesting")
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Kind != yaml.ScalarNode || node.Content[i].Tag != "!!str" {
				return errors.New("configuration keys must be strings")
			}
		}
	}
	for _, child := range node.Content {
		if err := strictConfigNode(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validate(base string) error {
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	if err := c.Worker.Policy.Validate(); err != nil {
		return err
	}
	if err := c.Scanner.Validate(); err != nil {
		return err
	}
	if err := c.Inspection.Validate(); err != nil {
		return err
	}
	if err := c.Scheduler.Validate(); err != nil {
		return err
	}
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
		if !library.ValidRootName(s.Name) || names[s.Name] {
			return errors.New("source names must be unique non-control labels of 1 to 64 UTF-8 bytes")
		}
		names[s.Name] = true
		if !fs.ValidPath(s.Marker) || s.Marker == "." || strings.ContainsAny(s.Marker, "\\\x00") {
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
