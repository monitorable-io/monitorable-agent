package cyclebatch

import (
	"errors"
	"time"
)

// Config is the cycle_batch processor's configuration.
type Config struct {
	// Deliveries is the number of ConsumeMetrics calls that make one complete
	// collection cycle: one per receiver in the pipeline. The group is sent as
	// soon as that many have arrived.
	Deliveries int `mapstructure:"deliveries"`
	// MaxWait bounds how long the first delivery of a group waits for the
	// rest. A group still incomplete after MaxWait is sent as it is.
	MaxWait time.Duration `mapstructure:"max_wait"`
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Deliveries < 1 {
		errs = append(errs, errors.New("cycle_batch: deliveries must be at least 1"))
	}
	if c.MaxWait <= 0 {
		errs = append(errs, errors.New("cycle_batch: max_wait must be greater than 0"))
	}
	return errors.Join(errs...)
}
