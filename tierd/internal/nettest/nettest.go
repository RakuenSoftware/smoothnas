package nettest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Request holds network test parameters supplied by the caller.
type Request struct {
	Type      string `json:"type"`      // local, external
	Host      string `json:"host"`      // local iperf3 target
	Port      int    `json:"port"`      // local iperf3 port
	DurationS int    `json:"duration"`  // seconds
	Mode      string `json:"mode"`      // upload, download
	Streams   int    `json:"streams"`   // iperf3 parallel streams
	ServerID  string `json:"server_id"` // external speedtest server
}

// DataPoint is a single chart sample from a network test run.
type DataPoint struct {
	ElapsedS     int     `json:"elapsed_s"`
	DownloadMbps float64 `json:"download_mbps"`
	UploadMbps   float64 `json:"upload_mbps"`
	LatencyMS    float64 `json:"latency_ms"`
}

// Result holds the parsed output for either a local or external network test.
type Result struct {
	Type           string      `json:"type"`
	Mode           string      `json:"mode,omitempty"`
	Target         string      `json:"target,omitempty"`
	DurationS      int         `json:"duration_sec"`
	DownloadMbps   float64     `json:"download_mbps"`
	UploadMbps     float64     `json:"upload_mbps"`
	LatencyMS      float64     `json:"latency_ms"`
	JitterMS       float64     `json:"jitter_ms,omitempty"`
	Retransmits    int         `json:"retransmits,omitempty"`
	PacketLoss     float64     `json:"packet_loss,omitempty"`
	ISP            string      `json:"isp,omitempty"`
	ExternalIP     string      `json:"external_ip,omitempty"`
	ServerName     string      `json:"server_name,omitempty"`
	ServerLocation string      `json:"server_location,omitempty"`
	ResultURL      string      `json:"result_url,omitempty"`
	DataPoints     []DataPoint `json:"data_points,omitempty"`
}

// Server describes an external speedtest server exposed to the UI.
type Server struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
	Host     string `json:"host,omitempty"`
	Label    string `json:"label"`
}

var safeHost = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)
var safeServerID = regexp.MustCompile(`^\d+$`)

// Validate checks request fields and returns a human-readable error or nil.
func (r *Request) Validate() error {
	switch r.Type {
	case "local":
		if !safeHost.MatchString(r.Host) {
			return fmt.Errorf("host must be a valid hostname or IP address")
		}
		if r.Port == 0 {
			r.Port = 5201
		}
		if r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
		if r.DurationS < 5 || r.DurationS > 120 {
			return fmt.Errorf("duration must be between 5 and 120 seconds")
		}
		if r.Streams == 0 {
			r.Streams = 1
		}
		if r.Streams < 1 || r.Streams > 16 {
			return fmt.Errorf("streams must be between 1 and 16")
		}
		if r.Mode == "" {
			r.Mode = "download"
		}
		if r.Mode != "download" && r.Mode != "upload" {
			return fmt.Errorf("mode must be upload or download")
		}
	case "external":
		if r.DurationS != 0 && (r.DurationS < 5 || r.DurationS > 120) {
			return fmt.Errorf("duration must be between 5 and 120 seconds")
		}
		if r.ServerID != "" && !safeServerID.MatchString(r.ServerID) {
			return fmt.Errorf("server_id must be numeric")
		}
	default:
		return fmt.Errorf("type must be local or external")
	}
	return nil
}

// Run executes a network test and returns parsed results.
func Run(req Request, progressFn func(string), resultFn func(*Result)) (*Result, error) {
	switch req.Type {
	case "local":
		return runLocal(req, progressFn, resultFn)
	case "external":
		return runExternal(req, progressFn, resultFn)
	default:
		return nil, fmt.Errorf("unsupported network test type %q", req.Type)
	}
}

// ListExternalServers returns the nearby speedtest servers reported by the CLI.
func ListExternalServers() ([]Server, error) {
	if _, err := exec.LookPath("speedtest"); err != nil {
		if hasSpeedtestConflict() {
			return nil, fmt.Errorf("official speedtest CLI is not available because the conflicting speedtest-cli package is installed")
		}
		return nil, fmt.Errorf("official speedtest CLI is not installed on this system")
	}

	cmd := speedtestCommand(nil, "--accept-license", "--accept-gdpr", "--servers", "--format=json")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, wrapCommandError("speedtest", err, raw)
	}
	return parseSpeedtestServers(raw)
}

func runLocal(req Request, progressFn func(string), resultFn func(*Result)) (*Result, error) {
	if _, err := exec.LookPath("iperf3"); err != nil {
		return nil, fmt.Errorf("iperf3 is not installed on this system")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.DurationS+30)*time.Second)
	defer cancel()

	args := []string{
		"-c", req.Host,
		"-p", strconv.Itoa(req.Port),
		"-t", strconv.Itoa(req.DurationS),
		"-P", strconv.Itoa(req.Streams),
		"-i", "1",
		"-J",
	}
	if req.Mode == "download" {
		args = append(args, "-R")
	}

	cmd := exec.CommandContext(ctx, "iperf3", args...)
	raw, err := runWithProgress(ctx, cmd, req.DurationS, "Running local network test", progressFn, nil)
	if err != nil {
		return nil, wrapCommandError("iperf3", err, raw)
	}

	result, err := parseIperf3Output(raw, req)
	if err != nil {
		return nil, err
	}
	if resultFn != nil {
		resultFn(result)
	}
	return result, nil
}

func runExternal(req Request, progressFn func(string), resultFn func(*Result)) (*Result, error) {
	if _, err := exec.LookPath("speedtest"); err != nil {
		if hasSpeedtestConflict() {
			return nil, fmt.Errorf("official speedtest CLI is not available because the conflicting speedtest-cli package is installed")
		}
		return nil, fmt.Errorf("official speedtest CLI is not installed on this system")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const progressDurationS = 25
	args := []string{"--accept-license", "--accept-gdpr", "--format=json", "--progress=no"}
	if req.ServerID != "" {
		args = append(args, "--server-id="+req.ServerID)
	}

	cmd := speedtestCommand(ctx, args...)
	raw, err := runWithProgress(ctx, cmd, progressDurationS, "Running external speed test", progressFn, func(elapsedS int) {
		if resultFn != nil {
			resultFn(buildExternalProgressResult(elapsedS, progressDurationS))
		}
	})
	if err != nil {
		return nil, wrapCommandError("speedtest", err, raw)
	}

	result, err := parseSpeedtestOutput(raw)
	if err != nil {
		return nil, err
	}
	if resultFn != nil {
		resultFn(result)
	}
	return result, nil
}

func runWithProgress(ctx context.Context, cmd *exec.Cmd, durationS int, label string, progressFn func(string), tickFn func(int)) ([]byte, error) {
	if progressFn != nil {
		progressFn(label + "...")
	}
	if tickFn != nil {
		tickFn(0)
	}

	done := make(chan struct{})
	if durationS > 0 && (progressFn != nil || tickFn != nil) {
		start := time.Now()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					elapsed := int(time.Since(start).Seconds())
					if elapsed > durationS {
						elapsed = durationS
					}
					if progressFn != nil {
						progressFn(fmt.Sprintf("%s... %ds / %ds", label, elapsed, durationS))
					}
					if tickFn != nil {
						tickFn(elapsed)
					}
				}
			}
		}()
	}

	raw, err := cmd.CombinedOutput()
	close(done)
	return raw, err
}

func wrapCommandError(name string, err error, raw []byte) error {
	out := strings.TrimSpace(string(raw))
	if out == "" {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return fmt.Errorf("%s failed: %s", name, out)
}

type iperf3Output struct {
	Intervals []struct {
		Sum struct {
			Start         float64 `json:"start"`
			End           float64 `json:"end"`
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
		} `json:"sum"`
	} `json:"intervals"`
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
		} `json:"sum_sent"`
		SumReceived struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_received"`
	} `json:"end"`
}

func parseIperf3Output(raw []byte, req Request) (*Result, error) {
	var out iperf3Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to parse iperf3 output: %w", err)
	}

	result := &Result{
		Type:      "local",
		Mode:      req.Mode,
		Target:    fmt.Sprintf("%s:%d", req.Host, req.Port),
		DurationS: req.DurationS,
	}

	for _, interval := range out.Intervals {
		point := DataPoint{ElapsedS: int(math.Round(interval.Sum.End))}
		mbps := interval.Sum.BitsPerSecond / 1_000_000
		if req.Mode == "download" {
			point.DownloadMbps = mbps
		} else {
			point.UploadMbps = mbps
		}
		result.DataPoints = append(result.DataPoints, point)
	}

	if req.Mode == "download" {
		result.DownloadMbps = out.End.SumReceived.BitsPerSecond / 1_000_000
	} else {
		result.UploadMbps = out.End.SumSent.BitsPerSecond / 1_000_000
		result.Retransmits = out.End.SumSent.Retransmits
	}

	return result, nil
}

type speedtestOutput struct {
	Ping struct {
		Latency float64 `json:"latency"`
		Jitter  float64 `json:"jitter"`
	} `json:"ping"`
	Download struct {
		Bandwidth float64 `json:"bandwidth"`
		ElapsedMS int     `json:"elapsed"`
	} `json:"download"`
	Upload struct {
		Bandwidth float64 `json:"bandwidth"`
		ElapsedMS int     `json:"elapsed"`
	} `json:"upload"`
	PacketLoss float64 `json:"packetLoss"`
	ISP        string  `json:"isp"`
	Interface  struct {
		ExternalIP string `json:"externalIp"`
	} `json:"interface"`
	Server struct {
		Name     string `json:"name"`
		Location string `json:"location"`
		Country  string `json:"country"`
	} `json:"server"`
	Result struct {
		URL string `json:"url"`
	} `json:"result"`
}

func parseSpeedtestOutput(raw []byte) (*Result, error) {
	var out speedtestOutput
	if err := decodeSpeedtestJSON(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to parse speedtest output: %w", err)
	}

	serverLocation := strings.TrimSpace(strings.Trim(strings.Join([]string{out.Server.Location, out.Server.Country}, ", "), ", "))
	downloadMbps := out.Download.Bandwidth * 8 / 1_000_000
	uploadMbps := out.Upload.Bandwidth * 8 / 1_000_000
	totalDuration := int(math.Ceil(float64(out.Download.ElapsedMS+out.Upload.ElapsedMS) / 1000))
	if totalDuration < 3 {
		totalDuration = 3
	}

	result := &Result{
		Type:           "external",
		DurationS:      totalDuration,
		DownloadMbps:   downloadMbps,
		UploadMbps:     uploadMbps,
		LatencyMS:      out.Ping.Latency,
		JitterMS:       out.Ping.Jitter,
		PacketLoss:     out.PacketLoss,
		ISP:            out.ISP,
		ExternalIP:     out.Interface.ExternalIP,
		ServerName:     out.Server.Name,
		ServerLocation: serverLocation,
		ResultURL:      out.Result.URL,
		DataPoints: []DataPoint{
			{ElapsedS: 1, LatencyMS: out.Ping.Latency},
			{ElapsedS: max(2, out.Download.ElapsedMS/1000), DownloadMbps: downloadMbps, LatencyMS: out.Ping.Latency},
			{ElapsedS: totalDuration, UploadMbps: uploadMbps, LatencyMS: out.Ping.Latency},
		},
	}

	return result, nil
}

func parseSpeedtestServers(raw []byte) ([]Server, error) {
	var root any
	if err := decodeSpeedtestJSON(raw, &root); err != nil {
		return nil, fmt.Errorf("failed to parse speedtest server list: %w", err)
	}

	var items []any
	switch v := root.(type) {
	case []any:
		items = v
	case map[string]any:
		switch {
		case asSlice(v["servers"]) != nil:
			items = asSlice(v["servers"])
		case asSlice(v["data"]) != nil:
			items = asSlice(v["data"])
		default:
			return nil, fmt.Errorf("speedtest server list did not contain a servers array")
		}
	default:
		return nil, fmt.Errorf("unexpected speedtest server list format")
	}

	servers := make([]Server, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		server := Server{
			ID:       asString(obj["id"]),
			Name:     firstNonEmpty(asString(obj["name"]), asString(obj["sponsor"])),
			Location: asString(obj["location"]),
			Country:  asString(obj["country"]),
			Host:     asString(obj["host"]),
		}
		if server.ID == "" {
			continue
		}
		server.Label = buildServerLabel(server)
		servers = append(servers, server)
	}

	if len(servers) == 0 {
		return nil, fmt.Errorf("speedtest server list was empty")
	}
	return servers, nil
}

func hasSpeedtestConflict() bool {
	cmd := exec.Command("dpkg-query", "-W", "-f", "${Status}", "speedtest-cli")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "install ok installed")
}

func buildExternalProgressResult(elapsedS, durationS int) *Result {
	if durationS < 1 {
		durationS = 1
	}
	if elapsedS < 0 {
		elapsedS = 0
	}
	if elapsedS > durationS {
		elapsedS = durationS
	}
	return &Result{
		Type:      "external",
		DurationS: durationS,
		DataPoints: []DataPoint{
			{ElapsedS: elapsedS},
		},
	}
}

func speedtestCommand(ctx context.Context, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if ctx != nil {
		cmd = exec.CommandContext(ctx, "speedtest", args...)
	} else {
		cmd = exec.Command("speedtest", args...)
	}
	cmd.Env = withEnvDefaults(os.Environ(),
		"HOME=/root",
		"USER=root",
		"LOGNAME=root",
	)
	return cmd
}

// decodeSpeedtestJSON handles the Ookla speedtest CLI's NDJSON output format.
// The CLI emits one JSON object per line (log, ping progress, download
// progress, upload progress, then the final result).  We scan all objects and
// decode the one whose "type" field is "result", falling back to the last
// decodable object when no typed result is found (e.g. legacy single-object
// output from older CLI versions).
func decodeSpeedtestJSON(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty output")
	}
	start := bytes.IndexAny(trimmed, "{[")
	if start < 0 {
		return fmt.Errorf("no JSON payload found")
	}

	// If the output is a plain JSON array (server list), decode it directly.
	if trimmed[start] == '[' {
		return json.NewDecoder(bytes.NewReader(trimmed[start:])).Decode(target)
	}

	// Walk every newline-delimited JSON object; pick type=="result" or last.
	var lastRaw json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(trimmed[start:]))
	for decoder.More() {
		var envelope struct {
			Type string          `json:"type"`
			Raw  json.RawMessage `json:"-"`
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			break
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Type == "result" {
			return json.Unmarshal(raw, target)
		}
		lastRaw = raw
	}
	if lastRaw == nil {
		return fmt.Errorf("no JSON payload found")
	}
	return json.Unmarshal(lastRaw, target)
}

func withEnvDefaults(env []string, defaults ...string) []string {
	for _, entry := range defaults {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		env = setEnvDefault(env, parts[0], parts[1])
	}
	return env
}

func setEnvDefault(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		if entry == prefix {
			env[i] = prefix + value
		}
		return env
	}
	return append(env, prefix+value)
}

func buildServerLabel(server Server) string {
	parts := []string{server.Name}
	location := strings.TrimSpace(strings.Trim(strings.Join([]string{server.Location, server.Country}, ", "), ", "))
	if location != "" {
		parts = append(parts, location)
	}
	if server.Host != "" {
		parts = append(parts, server.Host)
	}
	return strings.Join(parts, " - ")
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case float64:
		if value == math.Trunc(value) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
