package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func startCLIProcess(t *testing.T, binary, config, command string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), command+".log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, command, "--config", config)
	process.Stdout, process.Stderr = log, log
	if err := process.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = process.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					data, _ := os.ReadFile(logPath)
					t.Errorf("%s process failed: %v\n%s", command, err, data)
				}
			case <-time.After(15 * time.Second):
				_ = process.Process.Kill()
				<-done
				t.Errorf("%s process did not stop gracefully", command)
			}
			log.Close()
			if t.Failed() {
				data, readErr := os.ReadFile(logPath)
				t.Logf("%s process log after failed test (read error: %v):\n%s", command, readErr, data)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// Let the server's unchanged 10s metadata deadline report first, with bounded TLS
// overhead. Health probes keep their independent 1s budgets; no retry hides errors.
const scanHTTPSRequestTimeout = 12 * time.Second

func TestScanHTTPSClientBudgetCoversMetadataDeadline(t *testing.T) {
	if scanHTTPSRequestTimeout <= 10*time.Second || scanHTTPSRequestTimeout > 15*time.Second {
		t.Fatal("fixture timeout must be bounded and let the server metadata deadline report first")
	}
	// A six-second response is within the unchanged ten-second metadata contract.
	// This is a transport-budget unit, not mock ingestion or an API speed claim.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = scanHTTPSRequestTimeout
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("fixture client cancelled a response within the metadata contract", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("unexpected transport-budget response", response.StatusCode)
	}
}

type scanHTTPSClient struct {
	t            *testing.T
	client       *http.Client
	origin, csrf string
}

func (c *scanHTTPSClient) request(method, path string, body any, want int, result any, extra ...http.Header) http.Header {
	c.t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.origin+"/api/v2"+path, input)
	if err != nil {
		c.t.Fatal(err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", c.origin)
		req.Header.Set("Idempotency-Key", store.NewID())
		if c.csrf != "" {
			req.Header.Set("X-CSRF-Token", c.csrf)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for _, headers := range extra {
		for name, values := range headers {
			req.Header[name] = append([]string(nil), values...)
		}
	}
	started := time.Now()
	response, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(method, path, "after", time.Since(started), err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != want {
		c.t.Fatalf("%s %s: status=%d want=%d body=%s error=%v", method, path, response.StatusCode, want, data, err)
	}
	if result != nil {
		if raw, ok := result.(*[]byte); ok {
			*raw = data
		} else if err := json.Unmarshal(data, result); err != nil {
			c.t.Fatal(err)
		}
	}
	return response.Header
}

func (c *scanHTTPSClient) waitJob(id string) jobs.Job {
	c.t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		var job jobs.Job
		c.request("GET", "/admin/jobs/"+id, nil, 200, &job)
		if job.Status != "queued" && job.Status != "running" {
			return job
		}
		select {
		case <-deadline.C:
			c.t.Fatal("compiled scan did not settle", job, job.Counts)
		case <-tick.C:
		}
	}
}

func scanTIFF() []byte {
	data := make([]byte, 107)
	copy(data, "II")
	order := binary.LittleEndian
	order.PutUint16(data[2:4], 42)
	order.PutUint32(data[4:8], 8)
	order.PutUint16(data[8:10], 2)
	entry := func(offset int, tag, kind uint16, count, value uint32) {
		order.PutUint16(data[offset:offset+2], tag)
		order.PutUint16(data[offset+2:offset+4], kind)
		order.PutUint32(data[offset+4:offset+8], count)
		order.PutUint32(data[offset+8:offset+12], value)
	}
	entry(10, 0x0112, 3, 1, 6)
	entry(22, 0x8769, 4, 1, 38)
	order.PutUint16(data[38:40], 3)
	entry(40, 0x9003, 2, 20, 80)
	entry(52, 0x9011, 2, 7, 100)
	entry(64, 0x9291, 2, 4, 0x00333231)
	copy(data[80:100], "2026:11:05 12:34:56\x00")
	copy(data[100:107], "+08:00\x00")
	return data
}

func scanJPEGWithEXIF(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 12, 7)), nil); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), scanTIFF()...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	result := append([]byte{}, encoded.Bytes()[:2]...)
	result = append(result, segment...)
	result = append(result, payload...)
	return append(result, encoded.Bytes()[2:]...)
}

func scanAPNGWithEXIF(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	base := encoded.Bytes()
	var pixels []byte
	for offset := 8; offset < len(base); {
		length := int(binary.BigEndian.Uint32(base[offset:]))
		if string(base[offset+4:offset+8]) == "IDAT" {
			pixels = append(pixels, base[offset+8:offset+8+length]...)
		}
		offset += length + 12
	}
	result := []byte("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		result = binary.BigEndian.AppendUint32(result, uint32(len(data)))
		result = append(result, kind...)
		result = append(result, data...)
		checksum := crc32.NewIEEE()
		_, _ = checksum.Write([]byte(kind))
		_, _ = checksum.Write(data)
		result = binary.BigEndian.AppendUint32(result, checksum.Sum32())
	}
	chunk("IHDR", base[16:29])
	chunk("eXIf", scanTIFF())
	chunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 2})
	for index := uint32(0); index < 2; index++ {
		frame := make([]byte, 26)
		binary.BigEndian.PutUint32(frame, index)
		binary.BigEndian.PutUint32(frame[4:], 3)
		binary.BigEndian.PutUint32(frame[8:], 2)
		binary.BigEndian.PutUint16(frame[20:], 1)
		binary.BigEndian.PutUint16(frame[22:], uint16(10-index*5))
		chunk("fcTL", frame)
		if index == 0 {
			chunk("IDAT", pixels)
		} else {
			chunk("fdAT", append([]byte{0, 0, 0, 2}, pixels...))
		}
	}
	chunk("IEND", nil)
	return result
}

func scanWebPWithEXIF(t *testing.T) []byte {
	t.Helper()
	file, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "webp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if err := json.Unmarshal(file, &fixtures); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(fixtures["animated"])
	if err != nil || len(data) < 30 || string(data[12:16]) != "VP8X" {
		t.Fatal("invalid synthetic WebP fixture", err)
	}
	data[20] |= 8
	exif := scanTIFF()
	data = append(data, "EXIF"...)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(exif)))
	data = append(data, exif...)
	if len(exif)%2 != 0 {
		data = append(data, 0)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func TestCompiledCLIHTTPSNativeIngestionInEmbeddedAndStandaloneModes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gallerygo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, mode := range []struct{ embedded, scheduled bool }{{true, false}, {false, false}, {true, true}, {false, true}} {
		embedded, scheduled := mode.embedded, mode.scheduled
		t.Run(fmt.Sprintf("embedded=%t/scheduled=%t", embedded, scheduled), func(t *testing.T) {
			directory := t.TempDir()
			root := filepath.Join(directory, "source")
			if err := os.MkdirAll(filepath.Join(root, "Album"), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, ".mounted")
			if err := os.WriteFile(marker, []byte("isolated scan marker"), 0o400); err != nil {
				t.Fatal(err)
			}
			var input bytes.Buffer
			if err := png.Encode(&input, image.NewNRGBA(image.Rect(0, 0, 12, 7))); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(root, "Album", "photo.png")
			if err := os.WriteFile(name, input.Bytes(), 0o400); err != nil {
				t.Fatal(err)
			}
			exifInput := scanJPEGWithEXIF(t)
			exifName := filepath.Join(root, "Album", "capture.jpg")
			if err := os.WriteFile(exifName, exifInput, 0o400); err != nil {
				t.Fatal(err)
			}
			palette := color.Palette{color.Black, color.White}
			first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
			second := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
			second.Pix[0] = 1
			var animation bytes.Buffer
			if err := gif.EncodeAll(&animation, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 20}, LoopCount: 2}); err != nil {
				t.Fatal(err)
			}
			animationName := filepath.Join(root, "Album", "animation.gif")
			if err := os.WriteFile(animationName, animation.Bytes(), 0o400); err != nil {
				t.Fatal(err)
			}
			apngInput := scanAPNGWithEXIF(t)
			apngName := filepath.Join(root, "Album", "animation.png")
			if err := os.WriteFile(apngName, apngInput, 0o400); err != nil {
				t.Fatal(err)
			}
			webpInput := scanWebPWithEXIF(t)
			webpName := filepath.Join(root, "Album", "animation.webp")
			if err := os.WriteFile(webpName, webpInput, 0o400); err != nil {
				t.Fatal(err)
			}
			videos := scanVideoFixtures(t)
			for name, fixture := range videos {
				if err := os.WriteFile(filepath.Join(root, "Album", name), fixture.data, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			avifs := scanAVIFFixtures(t)
			for name, data := range avifs {
				if err := os.WriteFile(filepath.Join(root, "Album", name), data, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			tiffs := scanTIFFFixtures(t)
			for name, data := range tiffs {
				if err := os.WriteFile(filepath.Join(root, "Album", name), data, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			heifs := scanHEIFFixtures(t)
			for name, data := range heifs {
				if err := os.WriteFile(filepath.Join(root, "Album", name), data, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			mediaCount := 5 + len(videos) + len(avifs) + len(tiffs) + len(heifs)
			workUnits := int64(mediaCount * 3)
			assets := filepath.Join(directory, "web")
			if err := os.Mkdir(assets, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated API integration</title>"), 0o400); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			upstream, err := url.Parse("http://" + address)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
			defer proxy.Close()
			client := proxy.Client()
			client.Timeout = scanHTTPSRequestTimeout
			client.Jar, err = cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			settings := map[string]any{
				"listen": address, "base_url": proxy.URL, "database": filepath.Join(directory, "data", "gallerygo.db"),
				"cache_root": filepath.Join(directory, "cache"), "web_root": assets,
				"sources":   []map[string]string{{"name": "fixture", "root": root, "marker": ".mounted"}},
				"worker":    map[string]any{"embedded": embedded, "concurrency": 1, "poll": "100ms", "max_attempts": 2, "retry_base": "100ms", "retry_max": "200ms"},
				"scheduler": map[string]any{"enabled": scheduled},
			}
			config := filepath.Join(directory, "config.yaml")
			write := func(path string) {
				t.Helper()
				data, err := json.Marshal(settings)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(config)
			password := rand.Text()
			for _, args := range [][]string{{"migrate", "--config", config}, {"bootstrap-admin", "--config", config, "--username", "owner", "--password-stdin"}} {
				command := exec.CommandContext(t.Context(), binary, args...)
				if args[0] == "bootstrap-admin" {
					command.Stdin = bytes.NewBufferString(password + "\n")
				}
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v\n%s", args[0], err, output)
				}
			}
			stopServer := startCLIProcess(t, binary, config, "serve")
			defer stopServer()
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(25 * time.Millisecond)
			defer tick.Stop()
			probe := &http.Client{Timeout: time.Second}
			for {
				response, err := probe.Get(upstream.String() + "/api/v2/health/live")
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
						break
					}
				}
				select {
				case <-deadline.C:
					t.Fatal("compiled HTTP server did not start")
				case <-tick.C:
				}
			}
			api := &scanHTTPSClient{t: t, client: client, origin: proxy.URL}
			api.request("POST", "/auth/login", map[string]string{"username": "owner", "password": password}, 200, nil)
			var csrf struct {
				Token string `json:"token"`
			}
			api.request("GET", "/auth/csrf", nil, 200, &csrf)
			api.csrf = csrf.Token
			if !embedded {
				api.request("GET", "/health/ready", nil, 503, nil)
			}
			var collection struct {
				ID string `json:"id"`
			}
			api.request("POST", "/admin/libraries", map[string]string{"name": "Isolated native ingestion"}, 201, &collection)
			var source library.Source
			api.request("POST", "/admin/sources", map[string]any{"library_id": collection.ID, "root_name": "fixture", "minimum_files": 1}, 201, &source)
			var accepted jobs.Job
			if !scheduled {
				api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
			}
			var stopWorker func()
			probeConfig := config
			if !embedded {
				if !scheduled {
					var queued jobs.Job
					api.request("GET", "/admin/jobs/"+accepted.ID, nil, 200, &queued)
					if queued.Status != "queued" || queued.Attempts != 0 {
						t.Fatal("HTTP-only server consumed scan", queued)
					}
				}
				// The worker uses the same declared roots/DB but has no web assets and
				// must not try to bind the already occupied HTTP listener.
				settings["web_root"] = filepath.Join(directory, "absent-worker-assets")
				workerConfig := filepath.Join(directory, "worker.yaml")
				probeConfig = workerConfig
				write(workerConfig)
				stopWorker = startCLIProcess(t, binary, workerConfig, "worker")
				defer stopWorker()
			}
			if scheduled {
				automaticDeadline := time.NewTimer(10 * time.Second)
				defer automaticDeadline.Stop()
				for accepted.ID == "" {
					var history struct {
						Items []jobs.Job `json:"items"`
					}
					api.request("GET", "/admin/jobs", nil, 200, &history)
					if len(history.Items) == 1 {
						accepted = history.Items[0]
						break
					}
					select {
					case <-automaticDeadline.C:
						t.Fatal("source policy did not create a scheduled scan")
					case <-tick.C:
					}
				}
			}
			initialID := accepted.ID
			result := api.waitJob(accepted.ID)
			if result.Status != "succeeded" || result.Attempts != 1 || result.Succeeded != workUnits || result.Done != workUnits || !result.TotalKnown {
				t.Fatal("compiled native execution failed", result, result.Counts)
			}
			var readiness health.Readiness
			api.request("GET", "/health/ready", nil, 200, &readiness)
			if readiness.Status != "ok" || readiness.Worker != "ok" || readiness.Sources != "ok" {
				t.Fatal("committed worker/source evidence not ready", readiness)
			}
			for _, args := range [][]string{{"healthcheck", "--config", config}, {"healthcheck", "--worker", "--config", probeConfig}} {
				output, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %v\n%s", args, err, output)
				}
				if err := json.Unmarshal(output, &readiness); err != nil || readiness.Status != "ok" || readiness.Worker != "ok" {
					t.Fatal("CLI readiness did not check real dependencies", readiness, err)
				}
			}
			var albums struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			api.request("GET", "/albums", nil, 200, &albums)
			if len(albums.Items) != 1 {
				t.Fatal("scan did not expose exactly one authorized album", albums)
			}
			var medias struct {
				Items []media.Metadata `json:"items"`
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != mediaCount {
				t.Fatal("native metadata was not indexed", medias)
			}
			ids := make(map[string]string)
			snapshots := make(map[string]string)
			for _, item := range medias.Items {
				ids[item.FileName] = item.ID
				encoded, _ := json.Marshal(item)
				snapshots[item.FileName] = string(encoded)
				width, height := int64(12), int64(7)
				if item.FileName == "animation.gif" || item.FileName == "animation.png" || item.FileName == "animation.webp" {
					width, height = 3, 2
				}
				if item.Codec == nil || !media.ValidCodec(*item.Codec) {
					t.Fatal("trusted source codec was not indexed", item)
				}
				if data, ok := avifs[item.FileName]; ok {
					width, height = 16, 12
					if item.Kind != "image" || *item.Codec != "av1" || item.Animated == nil || item.FrameCount == nil || item.LoopCount == nil {
						t.Fatal("AVIF container evidence missing", item)
					}
					if item.FileName == "still.avif" {
						if *item.Animated || *item.FrameCount != 1 || *item.LoopCount != 1 || item.DurationMS != nil || item.Orientation == nil || *item.Orientation != 5 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
							t.Fatal("AVIF ordered transforms/EXIF were not published", item)
						}
					} else if item.FileName == "generic.avif" || item.FileName == "grid.avif" {
						if item.FileName == "grid.avif" {
							width, height = 96, 64
						}
						if *item.Animated || *item.FrameCount != 1 || *item.LoopCount != 1 || item.DurationMS != nil || item.Orientation != nil || item.CaptureAt != nil {
							t.Fatal("image-header profile fabricated static AVIF metadata", item)
						}
					} else if !*item.Animated || *item.FrameCount != 2 || *item.LoopCount != 2 || item.DurationMS == nil || *item.DurationMS != 400 || item.Orientation != nil || item.CaptureAt != nil {
						t.Fatal("AVIF total plays/cycle were guessed", item)
					}
					assertCompiledOriginal(t, api, item.ID, data, "image/avif")
				}
				if data, ok := heifs[item.FileName]; ok {
					width, height = 64, 64
					if item.Kind != "image" || *item.Codec != "hevc" || item.Animated == nil || *item.Animated || item.FrameCount == nil || *item.FrameCount != 1 || item.LoopCount == nil || *item.LoopCount != 1 || item.DurationMS != nil {
						t.Fatal("compiled HEIF did not preserve primary still evidence", item)
					}
					if item.FileName == "capture.heif" {
						width = 96
						if item.Orientation == nil || *item.Orientation != 5 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
							t.Fatal("compiled HEIF item transforms/EXIF were lost", item)
						}
					} else if item.Orientation != nil || item.CaptureAt != nil {
						t.Fatal("compiled HEIF invented capture metadata", item)
					}
					assertCompiledOriginal(t, api, item.ID, data, "image/heif")
				}
				if data, ok := tiffs[item.FileName]; ok {
					if item.Kind != "image" || *item.Codec != "tiff" || item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" || item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil || item.DurationMS != nil {
						t.Fatal("compiled TIFF metadata lost first-page EXIF or invented animation", item)
					}
					assertCompiledOriginal(t, api, item.ID, data, "image/tiff")
				}
				if fixture, ok := videos[item.FileName]; ok {
					width, height = 16, 12
					if item.Kind != "video" || *item.Codec != fixture.codec || item.DurationMS == nil || *item.DurationMS != 400 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != fixture.capture || item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil || fixture.orientation == 0 && item.Orientation != nil || fixture.orientation != 0 && (item.Orientation == nil || *item.Orientation != fixture.orientation) {
						t.Fatal("compiled video metadata lost or invented evidence", item)
					}
					assertCompiledOriginal(t, api, item.ID, fixture.data, fixture.mime)
				}
				if item.Width == nil || *item.Width != width || item.Height == nil || *item.Height != height {
					t.Fatal("encoded source dimensions changed", item)
				}
				if item.FileName == "photo.png" && (item.Orientation != nil || item.CaptureAt != nil) {
					t.Fatal("unknown PNG metadata was invented", item)
				}
			}
			assertCompiledMediaSelection(t, api, albums.Items[0].ID, medias.Items)
			id, exifID, animationID, apngID, webpID := ids["photo.png"], ids["capture.jpg"], ids["animation.gif"], ids["animation.png"], ids["animation.webp"]
			if id == "" || exifID == "" || animationID == "" || apngID == "" || webpID == "" {
				t.Fatal("source basenames were not preserved", medias)
			}
			var capture media.Metadata
			api.request("GET", "/media/"+exifID, nil, 200, &capture)
			wantCapture := time.Date(2026, 11, 5, 4, 34, 56, 123*int(time.Millisecond), time.UTC)
			if capture.Orientation == nil || *capture.Orientation != 6 || capture.CaptureAt == nil || !capture.CaptureAt.Equal(wantCapture) || capture.Revision == nil {
				t.Fatal("compiled JPEG inspection did not publish whitelisted UTC metadata", capture)
			}
			var animationMetadata media.Metadata
			api.request("GET", "/media/"+animationID, nil, 200, &animationMetadata)
			if animationMetadata.Animated == nil || !*animationMetadata.Animated || animationMetadata.FrameCount == nil || *animationMetadata.FrameCount != 2 || animationMetadata.LoopCount == nil || *animationMetadata.LoopCount != 3 || animationMetadata.DurationMS == nil || *animationMetadata.DurationMS != 300 || animationMetadata.Revision == nil {
				t.Fatal("compiled GIF metadata did not preserve normalized cycle evidence", animationMetadata)
			}
			var apngMetadata media.Metadata
			api.request("GET", "/media/"+apngID, nil, 200, &apngMetadata)
			if apngMetadata.Animated == nil || !*apngMetadata.Animated || apngMetadata.FrameCount == nil || *apngMetadata.FrameCount != 2 || apngMetadata.LoopCount == nil || *apngMetadata.LoopCount != 2 || apngMetadata.DurationMS == nil || *apngMetadata.DurationMS != 300 || apngMetadata.Orientation == nil || *apngMetadata.Orientation != 6 || apngMetadata.CaptureAt == nil || !apngMetadata.CaptureAt.Equal(wantCapture) || apngMetadata.Revision == nil {
				t.Fatal("compiled APNG lost EXIF or confused direct total plays with GIF repeats", apngMetadata)
			}
			var webpMetadata media.Metadata
			api.request("GET", "/media/"+webpID, nil, 200, &webpMetadata)
			if webpMetadata.Animated == nil || !*webpMetadata.Animated || webpMetadata.FrameCount == nil || *webpMetadata.FrameCount != 2 || webpMetadata.LoopCount == nil || *webpMetadata.LoopCount != 2 || webpMetadata.DurationMS == nil || *webpMetadata.DurationMS != 200 || webpMetadata.Orientation == nil || *webpMetadata.Orientation != 6 || webpMetadata.CaptureAt == nil || !webpMetadata.CaptureAt.Equal(wantCapture) || webpMetadata.Revision == nil {
				t.Fatal("compiled WebP lost EXIF or direct total plays", webpMetadata)
			}
			var original []byte
			headers := api.request("GET", "/media/"+webpID+"/original", nil, 200, &original)
			if !bytes.Equal(original, webpInput) || headers.Get("Content-Type") != "image/webp" {
				t.Fatal("WebP inspection rewrote protected original bytes", headers)
			}
			headers = api.request("GET", "/media/"+apngID+"/original", nil, 200, &original)
			if !bytes.Equal(original, apngInput) || headers.Get("Content-Type") != "image/png" {
				t.Fatal("APNG inspection rewrote protected original bytes", headers)
			}
			headers = api.request("GET", "/media/"+animationID+"/original", nil, 200, &original)
			if !bytes.Equal(original, animation.Bytes()) || headers.Get("Content-Type") != "image/gif" {
				t.Fatal("GIF metadata processing rewrote protected original bytes", headers)
			}
			headers = api.request("GET", "/media/"+exifID+"/original", nil, 200, &original)
			if !bytes.Equal(original, exifInput) || headers.Get("Content-Type") != "image/jpeg" {
				t.Fatal("EXIF processing rewrote protected original bytes", headers)
			}
			headers = api.request("GET", "/media/"+id+"/original", nil, 200, &original)
			if !bytes.Equal(original, input.Bytes()) || headers.Get("Content-Type") != "image/png" || headers.Get("Cache-Control") != "private, no-store" {
				t.Fatal("actual protected original changed", headers)
			}
			api.request("POST", "/admin/scans", map[string]string{"mode": "incremental", "source_id": source.ID}, 202, &accepted)
			if job := api.waitJob(accepted.ID); job.Status != "succeeded" {
				t.Fatal(job, job.Counts)
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != mediaCount {
				t.Fatal("repeat execution lost indexed media", medias)
			}
			for _, item := range medias.Items {
				if ids[item.FileName] != item.ID {
					t.Fatal("repeat execution duplicated logical media", item)
				}
				encoded, _ := json.Marshal(item)
				if string(encoded) != snapshots[item.FileName] {
					t.Fatal("repeat execution rewrote source-bound metadata or revisions", item)
				}
				if item.ID == exifID && (item.Revision == nil || *item.Revision != *capture.Revision || item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || !item.CaptureAt.Equal(wantCapture)) {
					t.Fatal("repeat execution lost source-bound EXIF", item)
				}
				if item.ID == animationID && (item.Revision == nil || *item.Revision != *animationMetadata.Revision || item.Animated == nil || !*item.Animated || item.FrameCount == nil || *item.FrameCount != 2 || item.LoopCount == nil || *item.LoopCount != 3 || item.DurationMS == nil || *item.DurationMS != 300) {
					t.Fatal("repeat execution lost source-bound animation", item)
				}
				if item.ID == apngID && (item.Revision == nil || *item.Revision != *apngMetadata.Revision || item.LoopCount == nil || *item.LoopCount != 2 || item.DurationMS == nil || *item.DurationMS != 300 || item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || !item.CaptureAt.Equal(wantCapture)) {
					t.Fatal("repeat execution lost source-bound APNG/EXIF", item)
				}
				if item.ID == webpID && (item.Revision == nil || *item.Revision != *webpMetadata.Revision || item.LoopCount == nil || *item.LoopCount != 2 || item.DurationMS == nil || *item.DurationMS != 200 || item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || !item.CaptureAt.Equal(wantCapture)) {
					t.Fatal("repeat execution lost source-bound WebP/EXIF", item)
				}
			}
			api.request("GET", "/admin/sources/"+source.ID, nil, 200, &source)
			generation := source.ScanGeneration
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
			if job := api.waitJob(accepted.ID); job.Status != "dead" || job.Attempts != 2 || job.Failed != 2 {
				t.Fatal("source outage was hidden", job, job.Counts)
			}
			var issuePage struct {
				Items      []jobs.Issue `json:"items"`
				NextCursor *string      `json:"next_cursor"`
				HasMore    bool         `json:"has_more"`
			}
			api.request("GET", "/admin/jobs/"+accepted.ID+"/issues?limit=1", nil, 200, &issuePage)
			if len(issuePage.Items) != 1 || !issuePage.HasMore || issuePage.NextCursor == nil {
				t.Fatal("compiled source failures did not expose bounded diagnostic history", issuePage)
			}
			firstIssue := issuePage.Items[0]
			api.request("GET", "/admin/jobs/"+accepted.ID+"/issues?limit=1&cursor="+*issuePage.NextCursor, nil, 200, &issuePage)
			if len(issuePage.Items) != 1 || issuePage.HasMore || issuePage.NextCursor != nil || issuePage.Items[0].ID == firstIssue.ID {
				t.Fatal("compiled diagnostic cursor lost or duplicated attempts", issuePage)
			}
			for _, issue := range []jobs.Issue{firstIssue, issuePage.Items[0]} {
				if issue.Attempt < 1 || issue.Attempt > 2 || issue.SourceID != source.ID || issue.SourceConfigVersion != source.ConfigVersion || issue.GenerationID == nil || issue.RelativePath == nil || *issue.RelativePath != "." || issue.ErrorCode != "SOURCE_UNAVAILABLE" || issue.Message == "" {
					t.Fatal("compiled scan diagnostics guessed or lost safe historical context", issue)
				}
			}
			api.request("GET", "/admin/sources/"+source.ID, nil, 200, &source)
			if source.ScanGeneration != generation || source.State != "unavailable" {
				t.Fatal("source outage blessed old inventory", source)
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != mediaCount {
				t.Fatal("source outage purged old index", medias)
			}
			for _, item := range medias.Items {
				if ids[item.FileName] != item.ID {
					t.Fatal("source outage replaced indexed identity", item)
				}
				encoded, _ := json.Marshal(item)
				if string(encoded) != snapshots[item.FileName] {
					t.Fatal("source outage erased trusted metadata or revisions", item)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("process recreated missing mount marker", err)
			}
			if data, err := os.ReadFile(name); err != nil || !bytes.Equal(data, input.Bytes()) {
				t.Fatal("process wrote source file", err)
			}
			if data, err := os.ReadFile(exifName); err != nil || !bytes.Equal(data, exifInput) {
				t.Fatal("process wrote EXIF source file", err)
			}
			if data, err := os.ReadFile(animationName); err != nil || !bytes.Equal(data, animation.Bytes()) {
				t.Fatal("process wrote GIF source file", err)
			}
			if data, err := os.ReadFile(apngName); err != nil || !bytes.Equal(data, apngInput) {
				t.Fatal("process wrote APNG/EXIF source file", err)
			}
			if data, err := os.ReadFile(webpName); err != nil || !bytes.Equal(data, webpInput) {
				t.Fatal("process wrote WebP/EXIF source file", err)
			}
			for name, fixture := range videos {
				path := filepath.Join(root, "Album", name)
				data, err := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if err != nil || statErr != nil || !bytes.Equal(data, fixture.data) || info.Mode().Perm() != 0o400 {
					t.Fatal("process changed read-only video source", name, err, statErr)
				}
			}
			for name, expected := range avifs {
				path := filepath.Join(root, "Album", name)
				data, err := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if err != nil || statErr != nil || !bytes.Equal(data, expected) || info.Mode().Perm() != 0o400 {
					t.Fatal("process changed read-only AVIF source", name, err, statErr)
				}
			}
			for name, expected := range heifs {
				path := filepath.Join(root, "Album", name)
				data, err := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if err != nil || statErr != nil || !bytes.Equal(data, expected) || info.Mode().Perm() != 0o400 {
					t.Fatal("process changed read-only HEIF source", name, err, statErr)
				}
			}
			for name, expected := range tiffs {
				path := filepath.Join(root, "Album", name)
				data, err := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if err != nil || statErr != nil || !bytes.Equal(data, expected) || info.Mode().Perm() != 0o400 {
					t.Fatal("process changed read-only TIFF source", name, err, statErr)
				}
			}
			api.request("GET", "/health/ready", nil, 200, &readiness)
			if readiness.Status != "degraded" || readiness.Sources != "degraded" || readiness.Worker != "ok" {
				t.Fatal("source failure hid metadata availability or worker status", readiness)
			}
			verifyCompiledExactDuplicates(t, api, source, marker, input.Bytes(), workUnits)
			verifyCompiledPersonMapping(t, api, source, marker, workUnits)
			verifyCompiledAlbumOverrides(t, api, source, marker, workUnits)
			verifyCompiledPersonOverrides(t, api, source, marker, workUnits)
			verifyCompiledTagCatalog(t, api)
			verifyCompiledAlbumTagAssociations(t, api, source, marker, workUnits)
			verifyCompiledCreatorOverrides(t, api, source, marker, workUnits)
			verifyCompiledTagBatchProcessing(t, api, source, marker)
			verifyCompiledCoverBatches(t, api, source, marker, workUnits)
			if stopWorker != nil {
				stopWorker()
				api.request("GET", "/health/ready", nil, 503, nil)
			}
			stopServer()
			if output, err := exec.CommandContext(t.Context(), binary, "healthcheck", "--worker", "--config", probeConfig).CombinedOutput(); err == nil {
				t.Fatal("CLI accepted stopped worker readiness", string(output))
			}
			db, err := store.Open(t.Context(), settings["database"].(string))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var batchIntents, batchTargets, succeededBatches, batchResults int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batches),
 (SELECT COUNT(*) FROM album_tag_batch_targets WHERE album_version>0 AND source_id IS NOT NULL AND source_config_version>0),
 (SELECT COUNT(*) FROM jobs WHERE kind='batch_tags' AND status='succeeded' AND attempts=1 AND total=2 AND done=2),
 (SELECT COUNT(*) FROM album_tag_batch_results WHERE outcome IN ('applied','unchanged') AND album_version>0)`).Scan(&batchIntents, &batchTargets, &succeededBatches, &batchResults); err != nil || batchIntents != 3 || batchTargets != 6 || succeededBatches != 3 || batchResults != 6 {
				t.Fatal("closed-process/reopened database lost applied batch intent/results", batchIntents, batchTargets, succeededBatches, batchResults, err)
			}
			var coverIntents, coverTargets, coverResults, completedCovers int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_cover_batches),(SELECT COUNT(*) FROM album_cover_batch_targets),(SELECT COUNT(*) FROM album_cover_batch_results),(SELECT COUNT(*) FROM jobs WHERE kind='batch_covers' AND status='succeeded' AND attempts=1 AND total=2 AND done=2)`).Scan(&coverIntents, &coverTargets, &coverResults, &completedCovers); err != nil || coverIntents != 3 || coverTargets != 6 || coverResults != 6 || completedCovers != 3 {
				t.Fatal("reopened database lost real cover batch outcomes", coverIntents, coverTargets, coverResults, completedCovers, err)
			}
			if scheduled {
				var actor *string
				var mode string
				if err := db.QueryRow(`SELECT created_by_user_id,json_extract(payload_json,'$.mode') FROM jobs WHERE id=?`, initialID).Scan(&actor, &mode); err != nil || actor != nil || mode != "full" {
					t.Fatal("automatic work used credentials or skipped initial full mode", actor, mode, err)
				}
			}
			role := "worker"
			if embedded {
				role = "serve"
			}
			var stopped, running, orphanEvents int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM workers WHERE role=? AND state='stopped'),(SELECT COUNT(*) FROM workers WHERE state='running'),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq))`, role).Scan(&stopped, &running, &orphanEvents); err != nil || stopped != 1 || running != 0 || orphanEvents != 0 {
				t.Fatal("process shutdown or transactional events incomplete", stopped, running, orphanEvents, err)
			}
		})
	}
}
