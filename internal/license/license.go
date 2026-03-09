package license

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/keygen-sh/keygen-go/v3"
)

const revalidateInterval = 6 * time.Hour

// Config holds the Keygen.sh credentials and the customer's license key.
type Config struct {
	Account    string // Keygen account ID (hardcoded in binary)
	Product    string // Keygen product ID (hardcoded in binary)
	PublicKey  string // Ed25519 public key (hardcoded in binary)
	LicenseKey string // Customer-provided license key (from env)
	Logger     *slog.Logger
}

// Status represents the current license state.
type Status struct {
	Valid         bool
	LicenseID     string
	Expiry        *time.Time
	GracePeriod   bool
	GraceDeadline *time.Time
	Detail        string
}

// Checker validates and monitors a Keygen.sh license.
type Checker struct {
	fingerprint string
	status      atomic.Value // *Status
	grace       *GraceTracker
	logger      *slog.Logger
}

// New creates a license checker. Call Start() to perform initial validation.
func New(cfg Config) (*Checker, error) {
	keygen.Account = cfg.Account
	keygen.Product = cfg.Product
	keygen.PublicKey = cfg.PublicKey
	keygen.LicenseKey = cfg.LicenseKey

	fingerprint, err := machineFingerprint(cfg.Product)
	if err != nil {
		return nil, fmt.Errorf("generating machine fingerprint: %w", err)
	}

	c := &Checker{
		fingerprint: fingerprint,
		grace:       NewGraceTracker(),
		logger:      cfg.Logger,
	}

	// Store initial invalid status
	c.status.Store(&Status{Detail: "not yet validated"})

	return c, nil
}

// Start performs initial license validation. Blocks startup on hard failures.
func (c *Checker) Start(ctx context.Context) error {
	license, err := keygen.Validate(ctx, c.fingerprint)

	switch {
	case err == nil:
		// Valid license
		c.grace.Clear()
		c.storeValid(license)
		c.logger.Info("license validated",
			"license_id", license.ID,
			"expiry", license.Expiry,
		)
		go c.revalidateLoop(ctx)
		return nil

	case errors.Is(err, keygen.ErrLicenseNotActivated):
		// First time on this machine — activate
		_, activateErr := license.Activate(ctx, c.fingerprint)
		if activateErr != nil {
			if errors.Is(activateErr, keygen.ErrMachineLimitExceeded) {
				return fmt.Errorf("machine limit exceeded — this license is already active on the maximum number of machines")
			}
			return fmt.Errorf("license activation failed: %w", activateErr)
		}
		c.grace.Clear()
		c.storeValid(license)
		c.logger.Info("license activated on this machine",
			"license_id", license.ID,
			"fingerprint", c.fingerprint,
		)
		go c.revalidateLoop(ctx)
		return nil

	case errors.Is(err, keygen.ErrLicenseExpired):
		return c.handleExpired(ctx, license)

	case errors.Is(err, keygen.ErrLicenseSuspended):
		return fmt.Errorf("license has been suspended — contact support")

	default:
		return fmt.Errorf("license validation failed: %w", err)
	}
}

// Status returns the current license status (thread-safe).
func (c *Checker) Status() *Status {
	return c.status.Load().(*Status)
}

// IsValid returns true if the license is valid or within its grace period.
func (c *Checker) IsValid() bool {
	s := c.Status()
	return s.Valid || s.GracePeriod
}

// LicenseDetail returns a human-readable status string for health endpoints.
func (c *Checker) LicenseDetail() string {
	return c.Status().Detail
}

func (c *Checker) handleExpired(ctx context.Context, license *keygen.License) error {
	if err := c.grace.RecordExpiry(); err != nil {
		c.logger.Warn("failed to record grace period", "error", err)
	}

	if c.grace.IsWithinGrace() {
		deadline, _ := c.grace.Deadline()
		c.status.Store(&Status{
			Valid:         false,
			LicenseID:     license.ID,
			Expiry:        license.Expiry,
			GracePeriod:   true,
			GraceDeadline: &deadline,
			Detail:        fmt.Sprintf("license expired — grace period until %s", deadline.Format(time.RFC3339)),
		})
		c.logger.Warn("license expired, running in grace period",
			"license_id", license.ID,
			"grace_deadline", deadline.Format(time.RFC3339),
		)
		go c.revalidateLoop(ctx)
		return nil
	}

	return fmt.Errorf("license expired and grace period has ended — please renew your license")
}

func (c *Checker) storeValid(license *keygen.License) {
	c.status.Store(&Status{
		Valid:     true,
		LicenseID: license.ID,
		Expiry:    license.Expiry,
		Detail:    "valid",
	})
}

func (c *Checker) revalidateLoop(ctx context.Context) {
	ticker := time.NewTicker(revalidateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.revalidate(ctx)
		}
	}
}

func (c *Checker) revalidate(ctx context.Context) {
	license, err := keygen.Validate(ctx, c.fingerprint)

	switch {
	case err == nil:
		c.grace.Clear()
		c.storeValid(license)
		c.logger.Debug("license revalidation passed", "license_id", license.ID)

	case errors.Is(err, keygen.ErrLicenseExpired):
		c.handleExpired(ctx, license)

	case errors.Is(err, keygen.ErrLicenseSuspended):
		c.status.Store(&Status{
			Valid:  false,
			Detail: "license suspended",
		})
		c.logger.Error("license has been suspended — service will become unhealthy")

	default:
		// Network error or transient failure — keep current status
		c.logger.Warn("license revalidation failed, keeping current status", "error", err)
	}
}
