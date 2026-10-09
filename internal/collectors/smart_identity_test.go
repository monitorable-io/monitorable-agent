package collectors

import (
	"errors"
	"strings"
	"testing"
)

func TestApplyIdentity_StripsNULPaddingAndSpaces(t *testing.T) {
	r := rawSmart{Device: "sda"}
	err := applyIdentity(&r, "\x00\x00Samsung SSD 870\x00 ", " S5Y2NX0R\x00\x00")
	if err != nil {
		t.Fatalf("applyIdentity error = %v", err)
	}
	if r.Model != "Samsung SSD 870" || r.Serial != "S5Y2NX0R" {
		t.Errorf("model=%q serial=%q", r.Model, r.Serial)
	}
}

func TestApplyIdentity_AllZeroIdentifyPageIsNotADisk(t *testing.T) {
	// Oracle Cloud's paravirtual BlockVolume answers ATA IDENTIFY with 512 zero
	// bytes: 40-byte NUL model, 20-byte NUL serial, capacity 0, no attributes.
	r := rawSmart{Device: "sda"}
	err := applyIdentity(&r, strings.Repeat("\x00", 40), strings.Repeat("\x00", 20))
	if !errors.Is(err, errEmptyIdentity) {
		t.Fatalf("err = %v, want errEmptyIdentity", err)
	}
	if r.Model != "" || r.Serial != "" {
		t.Errorf("model=%q serial=%q must not carry NUL bytes", r.Model, r.Serial)
	}
}

func TestApplyIdentity_ModelAloneIsEnough(t *testing.T) {
	r := rawSmart{Device: "sdb"}
	if err := applyIdentity(&r, "WDC WD40EFRX", ""); err != nil {
		t.Fatalf("model-only identity rejected: %v", err)
	}
	if err := applyIdentity(&r, "", "WD-WCC4E1234567"); err != nil {
		t.Fatalf("serial-only identity rejected: %v", err)
	}
}

func TestApplyIdentity_ControlCharsRemoved(t *testing.T) {
	r := rawSmart{Device: "sdb"}
	if err := applyIdentity(&r, "bad\x1b[31mmodel\x7f", "ok"); err != nil {
		t.Fatal(err)
	}
	if r.Model != "bad[31mmodel" {
		t.Errorf("model = %q", r.Model)
	}
}
