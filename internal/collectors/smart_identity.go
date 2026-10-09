package collectors

import (
	"errors"
	"fmt"
	"strings"
)

// errEmptyIdentity marks an identify page with neither model nor serial. That is
// not a drive: a paravirtual disk that answers ATA IDENTIFY / NVMe Identify with
// zeros instead of an error (Oracle Cloud's "ORACLE BlockVolume" returns 512 zero
// bytes, so the library classifies it as SATA and reports a 40-NUL model, a
// 20-NUL serial, capacity 0 and no attributes). Collect skips the device like any
// other unreadable one.
var errEmptyIdentity = errors.New("identify page carries no model or serial (virtual disk?)")

// cleanIdentString drops C0 control characters — NUL padding included, which
// the SMART library's string accessors leave in place (they trim spaces only) —
// and surrounding whitespace. Postgres rejects a NUL byte in VARCHAR and JSONB,
// so one such disk fails the server's whole SMART upsert on the backend.
func cleanIdentString(s string) string {
	return strings.TrimSpace(stripControlChars(s))
}

// applyIdentity sets r.Model and r.Serial from the raw identify strings and
// rejects a page that carries no identity at all.
func applyIdentity(r *rawSmart, model, serial string) error {
	r.Model = cleanIdentString(model)
	r.Serial = cleanIdentString(serial)
	if r.Model == "" && r.Serial == "" {
		return fmt.Errorf("%s: %w", r.Device, errEmptyIdentity)
	}
	return nil
}
