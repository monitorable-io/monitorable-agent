package monitorable

import (
	"testing"
)

func TestValidateDockerDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Docker.Mode != DockerModeAuto {
		t.Errorf("Mode = %q, want %q", cfg.Docker.Mode, DockerModeAuto)
	}
	if cfg.Docker.Endpoint != defaultDockerEndpoint {
		t.Errorf("Endpoint = %q, want %q", cfg.Docker.Endpoint, defaultDockerEndpoint)
	}
	if cfg.Docker.Timeout != defaultDockerTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Docker.Timeout, defaultDockerTimeout)
	}
}

func TestValidateDockerModeInvalid(t *testing.T) {
	cfg := &Config{Docker: DockerConfig{Mode: "bogus"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for invalid mode, got nil")
	}
}

func TestValidateDockerModeOnPreserved(t *testing.T) {
	cfg := &Config{Docker: DockerConfig{Mode: DockerModeOn}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Docker.Mode != DockerModeOn {
		t.Errorf("Mode = %q, want %q", cfg.Docker.Mode, DockerModeOn)
	}
}

func TestValidateDockerModeOffPreserved(t *testing.T) {
	cfg := &Config{Docker: DockerConfig{Mode: DockerModeOff}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Docker.Mode != DockerModeOff {
		t.Errorf("Mode = %q, want %q", cfg.Docker.Mode, DockerModeOff)
	}
}
