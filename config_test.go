package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "3djuggler.json")
	content := `{
		"InternEnpoint": {
			"api_uri": "https://example.com/3dprinters/api",
			"api_key": "secret",
			"api_app": "12345",
			"officeName": "Dublin DBC",
			"printerName": "North"
		},
		"Serial": "/dev/ttyACM1",
		"Listen": "[::1]:9999"
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if config.Listen != "[::1]:9999" {
		t.Errorf("Listen = %q", config.Listen)
	}
	if config.Serial != "/dev/ttyACM1" {
		t.Errorf("Serial = %q", config.Serial)
	}
	if config.InternEndpoint.PrinterName != "North" {
		t.Errorf("PrinterName = %q", config.InternEndpoint.PrinterName)
	}
	if config.InternEndpoint.APIKey != "secret" {
		t.Errorf("APIKey = %q", config.InternEndpoint.APIKey)
	}
}

// TestLoadConfigPreservesTypo guards the misspelled "InternEnpoint" key,
// which every deployed config file in the fleet still uses.
func TestLoadConfigPreservesTypo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	content := `{"InternEnpoint":{"api_uri":"https://x/api","api_app":"123","api_key":"k","printerName":"P","officeName":"O"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if config.InternEndpoint == nil {
		t.Fatal("InternEnpoint (with the historical typo) failed to parse")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	content := `{"InternEnpoint":{"api_uri":"https://x/api","api_app":"123","api_key":"k","printerName":"P","officeName":"O"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if config.Listen != defaultListen {
		t.Errorf("Listen = %q, want default %q", config.Listen, defaultListen)
	}
	if config.Serial != defaultSerial {
		t.Errorf("Serial = %q, want default %q", config.Serial, defaultSerial)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"missing section", `{"Listen":"[::1]:1"}`, "InternEnpoint"},
		{"missing api_uri", `{"InternEnpoint":{"api_app":"1","api_key":"k","printerName":"P","officeName":"O"}}`, "api_uri"},
		{"missing api_app", `{"InternEnpoint":{"api_uri":"https://x","api_key":"k","printerName":"P","officeName":"O"}}`, "api_app"},
		{"missing api_key", `{"InternEnpoint":{"api_uri":"https://x","api_app":"1","printerName":"P","officeName":"O"}}`, "api_key"},
		{"missing printer", `{"InternEnpoint":{"api_uri":"https://x","api_app":"1","api_key":"k","officeName":"O"}}`, "printerName"},
		{"missing office", `{"InternEnpoint":{"api_uri":"https://x","api_app":"1","api_key":"k","printerName":"P"}}`, "officeName"},
		{"malformed json", `{not json`, "can't decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

// TestInternEndpointStringRedactsKey is the regression test for the
// credential leak: the config used to be printed with %+v on every start,
// writing the API key to stdout and the log file.
func TestInternEndpointStringRedactsKey(t *testing.T) {
	ie := &InternEndpoint{
		APIApp:      "12345",
		APIKey:      "super-secret-token",
		APIURI:      "https://example.com/api",
		PrinterName: "North",
		OfficeName:  "Dublin DBC",
	}

	for _, format := range []string{"%v", "%s", "%+v"} {
		rendered := fmt.Sprintf(format, ie)
		if strings.Contains(rendered, "super-secret-token") {
			t.Errorf("format %s leaked the API key: %s", format, rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("format %s did not mark the key as redacted: %s", format, rendered)
		}
		if !strings.Contains(rendered, "North") {
			t.Errorf("format %s dropped useful fields: %s", format, rendered)
		}
	}
}
