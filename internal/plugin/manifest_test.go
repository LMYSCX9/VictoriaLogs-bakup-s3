package plugin

import (
	"errors"
	"testing"
)

// validManifest 返回一个通过 Validate 的最小合法 manifest，供表驱动破坏用。
func validManifest() *Manifest {
	return &Manifest{
		ManifestVersion: ManifestVersion,
		Name:            "vlogs-backup",
		DisplayName:     "VictoriaLogs 备份",
		Version:         "1.0.0",
		APIVersion:      "1.x",
		Modes:           []Mode{ModeTask},
		Binary:          Binary{Path: "vlogs-backup"},
		Tasks:           []Task{{Name: "probe", Title: "探测"}},
	}
}

func TestValidate_OK(t *testing.T) {
	m := validManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestValidate_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"wrong manifestVersion", func(m *Manifest) { m.ManifestVersion = 2 }},
		{"name uppercase", func(m *Manifest) { m.Name = "Vlogs" }},
		{"name too short", func(m *Manifest) { m.Name = "a" }},
		{"name starts with digit", func(m *Manifest) { m.Name = "1abc" }},
		{"displayName empty", func(m *Manifest) { m.DisplayName = "" }},
		{"version empty", func(m *Manifest) { m.Version = "" }},
		{"apiVersion empty", func(m *Manifest) { m.APIVersion = "" }},
		{"modes empty", func(m *Manifest) { m.Modes = nil }},
		{"mode invalid", func(m *Manifest) { m.Modes = []Mode{"worker"} }},
		{"task mode without tasks", func(m *Manifest) { m.Tasks = nil }},
		{"duplicate task name", func(m *Manifest) {
			m.Tasks = []Task{{Name: "probe", Title: "a"}, {Name: "probe", Title: "b"}}
		}},
		{"task name invalid", func(m *Manifest) { m.Tasks = []Task{{Name: "Probe!"}} }},
		{"binary path empty", func(m *Manifest) { m.Binary.Path = "" }},
		{"ui without daemon", func(m *Manifest) { m.UI = &UI{Entries: []UIEntry{{Kind: "iframe", Title: "x", Path: "/x"}}} }},
		{"config invalid type", func(m *Manifest) {
			m.Config = map[string]ConfigItem{"k": {Type: "float"}}
		}},
		{"config enum without values", func(m *Manifest) {
			m.Config = map[string]ConfigItem{"k": {Type: "enum"}}
		}},
		{"config key invalid", func(m *Manifest) {
			m.Config = map[string]ConfigItem{"Bad-Key": {Type: "string"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validManifest()
			tt.mutate(m)
			if err := m.Validate(); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestValidate_DaemonOnlyMode(t *testing.T) {
	m := validManifest()
	m.Modes = []Mode{ModeDaemon}
	m.Tasks = nil
	m.Service = &Service{Protocol: "http.v1"}
	if err := m.Validate(); err != nil {
		t.Fatalf("daemon-only manifest rejected: %v", err)
	}
}

func TestParseHandshake(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    *HandshakeLine
		wantErr bool
	}{
		{
			name: "valid tcp line",
			line: "OPS-PLUGIN-1|http.v1|tcp|127.0.0.1:39127|dG9rZW4|1234",
			want: &HandshakeLine{Protocol: "http.v1", Network: "tcp", Addr: "127.0.0.1:39127", Token: "dG9rZW4", PID: 1234},
		},
		{name: "missing prefix", line: "HELLO|http.v1|tcp|a|b|1", wantErr: true},
		{name: "too few fields", line: "OPS-PLUGIN-1|http.v1|tcp|addr", wantErr: true},
		{name: "pid not a number", line: "OPS-PLUGIN-1|http.v1|tcp|a|b|pid", wantErr: true},
		{name: "empty", line: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHandshake(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tt.line, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHandshake(%q): %v", tt.line, err)
			}
			if *got != *tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateListenAddr(t *testing.T) {
	if err := ValidateListenAddr("tcp", "127.0.0.1:9000"); err != nil {
		t.Errorf("loopback tcp rejected: %v", err)
	}
	if err := ValidateListenAddr("tcp", "0.0.0.0:9000"); err == nil {
		t.Error("non-loopback tcp accepted")
	}
	if err := ValidateListenAddr("tcp", "192.168.1.5:9000"); err == nil {
		t.Error("non-loopback tcp accepted")
	}
	if err := ValidateListenAddr("unix", "/tmp/plugin.sock"); err != nil {
		t.Errorf("unix socket rejected: %v", err)
	}
	if err := ValidateListenAddr("pipe", "vlogs-backup-plugin"); err != nil {
		t.Errorf("named pipe rejected: %v", err)
	}
	if err := ValidateListenAddr("foo", "x"); err == nil {
		t.Error("unknown network accepted")
	}
}

func TestPluginEnv(t *testing.T) {
	t.Run("not in plugin env", func(t *testing.T) {
		t.Setenv(EnvPlugin, "")
		t.Setenv(EnvPluginMagic, "")
		if IsPluginEnv() {
			t.Fatal("IsPluginEnv = true without env")
		}
		if _, err := ReadPluginEnv(); !errors.Is(err, ErrNotInPluginEnv) {
			t.Fatalf("err = %v, want ErrNotInPluginEnv", err)
		}
	})
	t.Run("magic mismatch", func(t *testing.T) {
		t.Setenv(EnvPlugin, "1")
		t.Setenv(EnvPluginMagic, "WRONG")
		if IsPluginEnv() {
			t.Fatal("IsPluginEnv = true with wrong magic")
		}
	})
	t.Run("full env", func(t *testing.T) {
		t.Setenv(EnvPlugin, "1")
		t.Setenv(EnvPluginMagic, MagicValue)
		t.Setenv(EnvPluginName, "vlogs-backup")
		t.Setenv(EnvPluginInst, "inst-1")
		t.Setenv(EnvPluginData, `C:\data\vlogs-backup`)
		t.Setenv(EnvPluginConfig, `{"a":1}`)
		t.Setenv(EnvPluginLogLv, "debug")
		if !IsPluginEnv() {
			t.Fatal("IsPluginEnv = false with valid env")
		}
		env, err := ReadPluginEnv()
		if err != nil {
			t.Fatalf("ReadPluginEnv: %v", err)
		}
		if env.Name != "vlogs-backup" || env.Instance != "inst-1" || env.DataDir != `C:\data\vlogs-backup` ||
			env.Config != `{"a":1}` || env.LogLevel != "debug" {
			t.Errorf("env snapshot mismatch: %+v", env)
		}
	})
}
