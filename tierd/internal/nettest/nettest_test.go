package nettest

import (
	"context"
	"testing"
)

func TestRequestValidateLocalDefaults(t *testing.T) {
	req := Request{
		Type:      "local",
		Host:      "192.168.1.10",
		DurationS: 15,
	}

	if err := req.Validate(); err != nil {
		t.Fatalf("validate local request: %v", err)
	}
	if req.Port != 5201 {
		t.Fatalf("expected default port 5201, got %d", req.Port)
	}
	if req.Streams != 1 {
		t.Fatalf("expected default streams 1, got %d", req.Streams)
	}
	if req.Mode != "download" {
		t.Fatalf("expected default mode download, got %q", req.Mode)
	}
}

func TestRequestValidateRejectsBadExternalServerID(t *testing.T) {
	req := Request{Type: "external", ServerID: "abc"}
	if err := req.Validate(); err == nil {
		t.Fatal("expected invalid server ID to fail")
	}
}

func TestParseIperf3OutputUpload(t *testing.T) {
	raw := []byte(`{
		"intervals":[
			{"sum":{"start":0,"end":1,"bits_per_second":8000000000,"retransmits":1}},
			{"sum":{"start":1,"end":2,"bits_per_second":8400000000,"retransmits":0}}
		],
		"end":{
			"sum_sent":{"bits_per_second":8200000000,"retransmits":3},
			"sum_received":{"bits_per_second":8180000000}
		}
	}`)

	result, err := parseIperf3Output(raw, Request{
		Type:      "local",
		Host:      "nas-lan",
		Port:      5201,
		DurationS: 10,
		Mode:      "upload",
	})
	if err != nil {
		t.Fatalf("parse iperf3 upload output: %v", err)
	}

	if result.UploadMbps != 8200 {
		t.Fatalf("expected upload Mbps 8200, got %v", result.UploadMbps)
	}
	if result.DownloadMbps != 0 {
		t.Fatalf("expected no download Mbps, got %v", result.DownloadMbps)
	}
	if result.Retransmits != 3 {
		t.Fatalf("expected retransmits 3, got %d", result.Retransmits)
	}
	if len(result.DataPoints) != 2 {
		t.Fatalf("expected 2 datapoints, got %d", len(result.DataPoints))
	}
	if result.DataPoints[0].UploadMbps != 8000 {
		t.Fatalf("expected first point upload 8000, got %v", result.DataPoints[0].UploadMbps)
	}
}

func TestParseIperf3OutputDownload(t *testing.T) {
	raw := []byte(`{
		"intervals":[
			{"sum":{"start":0,"end":1,"bits_per_second":9400000000}},
			{"sum":{"start":1,"end":2,"bits_per_second":9600000000}}
		],
		"end":{
			"sum_sent":{"bits_per_second":1000000,"retransmits":0},
			"sum_received":{"bits_per_second":9500000000}
		}
	}`)

	result, err := parseIperf3Output(raw, Request{
		Type:      "local",
		Host:      "nas-lan",
		Port:      5201,
		DurationS: 10,
		Mode:      "download",
	})
	if err != nil {
		t.Fatalf("parse iperf3 download output: %v", err)
	}

	if result.DownloadMbps != 9500 {
		t.Fatalf("expected download Mbps 9500, got %v", result.DownloadMbps)
	}
	if result.DataPoints[1].DownloadMbps != 9600 {
		t.Fatalf("expected second point download 9600, got %v", result.DataPoints[1].DownloadMbps)
	}
}

func TestParseSpeedtestOutput(t *testing.T) {
	raw := []byte(`{
		"ping":{"latency":8.42,"jitter":1.13},
		"download":{"bandwidth":25000000,"elapsed":15000},
		"upload":{"bandwidth":12500000,"elapsed":10000},
		"packetLoss":0,
		"isp":"Example Fiber",
		"interface":{"externalIp":"203.0.113.10"},
		"server":{"name":"Example Server","location":"Denver","country":"United States"},
		"result":{"url":"https://www.speedtest.net/result/c/example"}
	}`)

	result, err := parseSpeedtestOutput(raw)
	if err != nil {
		t.Fatalf("parse speedtest output: %v", err)
	}

	if result.DownloadMbps != 200 {
		t.Fatalf("expected download Mbps 200, got %v", result.DownloadMbps)
	}
	if result.UploadMbps != 100 {
		t.Fatalf("expected upload Mbps 100, got %v", result.UploadMbps)
	}
	if result.LatencyMS != 8.42 {
		t.Fatalf("expected latency 8.42, got %v", result.LatencyMS)
	}
	if result.ServerLocation != "Denver, United States" {
		t.Fatalf("expected joined server location, got %q", result.ServerLocation)
	}
	if len(result.DataPoints) != 3 {
		t.Fatalf("expected 3 synthetic datapoints, got %d", len(result.DataPoints))
	}
	if result.DataPoints[1].DownloadMbps != 200 {
		t.Fatalf("expected phase datapoint download 200, got %v", result.DataPoints[1].DownloadMbps)
	}
}

func TestParseSpeedtestOutputIgnoresProgressPreamble(t *testing.T) {
	raw := []byte(`========== speedtest progress ==========
{"ping":{"latency":8.42,"jitter":1.13},"download":{"bandwidth":25000000,"elapsed":15000},"upload":{"bandwidth":12500000,"elapsed":10000},"packetLoss":0,"isp":"Example Fiber","interface":{"externalIp":"203.0.113.10"},"server":{"name":"Example Server","location":"Denver","country":"United States"},"result":{"url":"https://www.speedtest.net/result/c/example"}}`)

	result, err := parseSpeedtestOutput(raw)
	if err != nil {
		t.Fatalf("parse speedtest output with preamble: %v", err)
	}
	if result.ServerName != "Example Server" {
		t.Fatalf("expected server name to parse, got %q", result.ServerName)
	}
}

func TestParseSpeedtestOutputNDJSON(t *testing.T) {
	// The Ookla CLI emits one JSON object per line even with --progress=no.
	// Only the final object has "type":"result"; earlier ones must be skipped.
	raw := []byte(`{"type":"log","timestamp":"2026-04-09T00:00:00Z","message":"Configuration - ","level":"info"}
{"type":"ping","timestamp":"2026-04-09T00:00:01Z","ping":{"jitter":1.13,"latency":8.42,"progress":0.75}}
{"type":"download","timestamp":"2026-04-09T00:00:10Z","download":{"bandwidth":12500000,"bytes":125000000,"elapsed":10000,"progress":0.5}}
{"type":"upload","timestamp":"2026-04-09T00:00:20Z","upload":{"bandwidth":6250000,"bytes":62500000,"elapsed":10000,"progress":0.5}}
{"type":"result","timestamp":"2026-04-09T00:00:25Z","ping":{"jitter":1.13,"latency":8.42},"download":{"bandwidth":25000000,"bytes":250000000,"elapsed":15000},"upload":{"bandwidth":12500000,"bytes":125000000,"elapsed":10000},"packetLoss":0,"isp":"Example Fiber","interface":{"externalIp":"203.0.113.10"},"server":{"name":"Example Server","location":"Denver","country":"United States"},"result":{"url":"https://www.speedtest.net/result/c/example"}}`)

	result, err := parseSpeedtestOutput(raw)
	if err != nil {
		t.Fatalf("parse NDJSON speedtest output: %v", err)
	}
	if result.DownloadMbps != 200 {
		t.Fatalf("expected download 200 Mbps, got %v", result.DownloadMbps)
	}
	if result.UploadMbps != 100 {
		t.Fatalf("expected upload 100 Mbps, got %v", result.UploadMbps)
	}
	if result.LatencyMS != 8.42 {
		t.Fatalf("expected latency 8.42 ms, got %v", result.LatencyMS)
	}
	if result.ServerName != "Example Server" {
		t.Fatalf("expected server name, got %q", result.ServerName)
	}
}

func TestParseSpeedtestServers(t *testing.T) {
	raw := []byte(`{
		"servers":[
			{"id":1234,"name":"Fiber Hub","location":"Denver","country":"United States","host":"denver.example.net"},
			{"id":"5678","name":"Mountain ISP","location":"Boulder","country":"United States","host":"boulder.example.net"}
		]
	}`)

	servers, err := parseSpeedtestServers(raw)
	if err != nil {
		t.Fatalf("parse speedtest servers: %v", err)
	}

	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
	if servers[0].ID != "1234" {
		t.Fatalf("expected numeric server ID to be normalized, got %q", servers[0].ID)
	}
	if servers[0].Label != "Fiber Hub - Denver, United States - denver.example.net" {
		t.Fatalf("unexpected server label %q", servers[0].Label)
	}
}

func TestSpeedtestCommandSetsFallbackEnv(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")

	cmd := speedtestCommand(context.Background(), "--format=json")

	if got := envValue(cmd.Env, "HOME"); got != "/root" {
		t.Fatalf("expected HOME=/root, got %q", got)
	}
	if got := envValue(cmd.Env, "USER"); got != "root" {
		t.Fatalf("expected USER=root, got %q", got)
	}
	if got := envValue(cmd.Env, "LOGNAME"); got != "root" {
		t.Fatalf("expected LOGNAME=root, got %q", got)
	}
}

func TestSpeedtestCommandPreservesExistingEnv(t *testing.T) {
	t.Setenv("HOME", "/srv/tierd")
	t.Setenv("USER", "smoothnas")
	t.Setenv("LOGNAME", "smoothnas")

	cmd := speedtestCommand(context.Background(), "--format=json")

	if got := envValue(cmd.Env, "HOME"); got != "/srv/tierd" {
		t.Fatalf("expected HOME to be preserved, got %q", got)
	}
	if got := envValue(cmd.Env, "USER"); got != "smoothnas" {
		t.Fatalf("expected USER to be preserved, got %q", got)
	}
	if got := envValue(cmd.Env, "LOGNAME"); got != "smoothnas" {
		t.Fatalf("expected LOGNAME to be preserved, got %q", got)
	}
}

func TestBuildExternalProgressResult(t *testing.T) {
	result := buildExternalProgressResult(11, 25)
	if result.Type != "external" {
		t.Fatalf("expected external type, got %q", result.Type)
	}
	if result.DurationS != 25 {
		t.Fatalf("expected duration 25, got %d", result.DurationS)
	}
	if len(result.DataPoints) != 1 || result.DataPoints[0].ElapsedS != 11 {
		t.Fatalf("unexpected progress datapoints: %+v", result.DataPoints)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if len(entry) > len(prefix) && entry[:len(prefix)] == prefix {
			return entry[len(prefix):]
		}
		if entry == prefix {
			return ""
		}
	}
	return ""
}
