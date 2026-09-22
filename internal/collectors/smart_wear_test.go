package collectors

import "testing"

func TestVendorWearAttrID(t *testing.T) {
	cases := map[string]uint8{
		"Samsung SSD 860 EVO 1TB": 177,
		"samsung ssd":             177, // case-insensitive
		"INTEL SSDSC2KW256G8":     233,
		"Crucial_CT1050MX300SSD1": 202,
		"WDC WDS100T2B0A-00SM50":  231, // WDC matched before bare WD
		"WD Blue SA510 2.5 1TB":   231,
		"KINGSTON SA400S37240G":   231,
		"TOSHIBA THNSNK256GVN8":   233,
		"SanDisk SDSSDH3 1T00":    232,
		"CT500MX500SSD1":          0, // raw Crucial model w/o brand string → unknown (falls back)
		"SomeUnknownDisk 9000":    0,
	}
	for model, want := range cases {
		if got := vendorWearAttrID(model); got != want {
			t.Errorf("vendorWearAttrID(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestSelectAtaWearRemaining(t *testing.T) {
	// vendor attribute present → use it (Samsung → 177), even though others are present
	if w := selectAtaWearRemaining("Samsung SSD 860 EVO", map[uint8]int{177: 95, 231: 80}); w == nil || *w != 95 {
		t.Errorf("vendor attr: got %v, want 95 (attr 177)", w)
	}
	// vendor known but its mapped attribute absent → nil (no generic fallback)
	if w := selectAtaWearRemaining("Samsung SSD", map[uint8]int{231: 88, 233: 70}); w != nil {
		t.Errorf("mapped vendor, attr absent: got %v, want nil", w)
	}
	// vendor unknown → nil (generic cross-vendor fallback removed)
	if w := selectAtaWearRemaining("CT500MX500SSD1", map[uint8]int{233: 60, 231: 90}); w != nil {
		t.Errorf("unknown vendor: got %v, want nil", w)
	}
	// no life attribute present → nil
	if w := selectAtaWearRemaining("Whatever", map[uint8]int{9: 100, 194: 30}); w != nil {
		t.Errorf("no life attr: got %v, want nil", w)
	}
}

func TestWearoutUsedPctPrecedence(t *testing.T) {
	devstat := 51
	remaining := 98 // would yield 2% used via the attr path
	// devstat present → wins over the vendor-attr remaining-life value
	r := rawSmart{AtaDevstatPercentUsed: &devstat, AtaWearRemaining: &remaining}
	if w := wearoutUsedPct(r); w == nil || *w != 51 {
		t.Errorf("devstat-first: got %v, want 51", w)
	}
	// no devstat, mapped-vendor attr present → 100 - remaining
	r = rawSmart{AtaWearRemaining: &remaining}
	if w := wearoutUsedPct(r); w == nil || *w != 2 {
		t.Errorf("attr fallback: got %v, want 2", w)
	}
	// neither devstat nor attr → nil (unknown)
	r = rawSmart{}
	if w := wearoutUsedPct(r); w != nil {
		t.Errorf("unknown: got %v, want nil", w)
	}
}
