package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/store"
)

const defaultPageSize = 100

type durableWork interface {
	ClaimCollectionJob(context.Context, string, time.Duration) (store.JobClaim, bool, error)
	RenewCollectionLease(context.Context, store.JobClaim, time.Duration) error
	PersistCollectionPage(context.Context, store.JobClaim, event.Page, event.FetchReport, bool, int, uuid.UUID) (bool, error)
	DeferCollectionJob(context.Context, store.JobClaim, time.Time) error
	RetryCollectionJob(context.Context, store.JobClaim, error, time.Time) error
	FailCollectionJob(context.Context, store.JobClaim, string) error
	ReleaseCollectionJob(context.Context, store.JobClaim) error
	ClaimNormalizationJob(context.Context, string, int, time.Duration) (store.NormalizationClaim, bool, error)
	RenewNormalizationLease(context.Context, store.NormalizationClaim, time.Duration) error
	CompleteNormalization(context.Context, store.NormalizationClaim, []event.Action) error
	RetryNormalizationJob(context.Context, store.NormalizationClaim, error, time.Time) error
	FailNormalizationJob(context.Context, store.NormalizationClaim, string) error
	ReserveRateLimit(context.Context, string, store.JobClaim, time.Duration) (store.QuotaReservation, error)
	ReleaseRateLimit(context.Context, uuid.UUID) error
}

// Connector fetches one bounded page for a source.
type Connector interface {
	Source() event.Source
	QuotaBucket() string
	FetchPage(context.Context, event.PageRequest) (event.Page, event.FetchReport, error)
}

// Normalizer transforms one committed raw event into normalized actions.
type Normalizer interface {
	Normalize(event.RawEvent) ([]event.Action, error)
}

// RunnerOptions configures one collector worker.
type RunnerOptions struct {
	Owner             string
	LeaseDuration     time.Duration
	PollInterval      time.Duration
	PageSize          int
	NormalizerVersion int
}

// Runner processes durable collection and normalization work.
type Runner struct {
	store      durableWork
	connectors map[event.Source]Connector
	normalizer Normalizer
	logger     *slog.Logger
	options    RunnerOptions
}

// ErrInvalidConnectorPage indicates a connector violated the bounded page contract.
var ErrInvalidConnectorPage = errors.New("invalid connector page")

// NewRunner constructs a worker with its source connectors and normalizer.
func NewRunner(s durableWork, connectors []Connector, transformer Normalizer, logger *slog.Logger, options RunnerOptions) (*Runner, error) {
	if s == nil || transformer == nil {
		return nil, errors.New("collector runner requires a store and normalizer")
	}
	if options.Owner == "" {
		return nil, errors.New("collector runner owner is required")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 2 * time.Minute
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.PageSize == 0 {
		options.PageSize = defaultPageSize
	}
	if options.NormalizerVersion == 0 {
		options.NormalizerVersion = 1
	}
	if options.LeaseDuration < 3*time.Millisecond || options.PollInterval <= 0 ||
		options.PageSize < 1 || options.PageSize > 500 || options.NormalizerVersion < 1 {
		return nil, errors.New("collector runner options must be positive and page size must not exceed 500")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	registry := make(map[event.Source]Connector, len(connectors))
	for _, connector := range connectors {
		if connector == nil || connector.Source() == "" {
			return nil, errors.New("collector connector requires a source")
		}
		if _, exists := registry[connector.Source()]; exists {
			return nil, fmt.Errorf("duplicate collector connector for %q", connector.Source())
		}
		registry[connector.Source()] = connector
	}
	return &Runner{
		store: s, connectors: registry, normalizer: transformer,
		logger: logger, options: options,
	}, nil
}

// RunOnce processes one normalization item or one complete source scan.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	normalization, found, err := r.store.ClaimNormalizationJob(
		ctx, r.options.Owner, r.options.NormalizerVersion, r.options.LeaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim normalization work: %w", err)
	}
	if found {
		return true, r.processNormalization(ctx, normalization)
	}
	claim, found, err := r.store.ClaimCollectionJob(ctx, r.options.Owner, r.options.LeaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim collection work: %w", err)
	}
	if !found {
		return false, nil
	}
	return true, r.processCollection(ctx, claim)
}

// Drain processes due work until the store has no immediately available jobs.
func (r *Runner) Drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		worked, err := r.RunOnce(ctx)
		if err != nil {
			return err
		}
		if !worked {
			return nil
		}
	}
}

// Run polls for durable work until the context is canceled. A failed poll,
// such as a database outage, is logged and retried after the poll interval so
// the worker recovers without a restart; leases protect interrupted work.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	for {
		worked, err := r.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logger.ErrorContext(ctx, "collection poll failed; retrying", "err", err)
			worked = false
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runner) processCollection(ctx context.Context, claim store.JobClaim) error {
	source := event.Source(claim.SourceKey)
	connector, ok := r.connectors[source]
	if !ok {
		return r.store.FailCollectionJob(ctx, claim, "no connector registered for source")
	}
	for {
		if err := ctx.Err(); err != nil {
			return r.releaseCanceledJob(ctx, claim, err)
		}
		if err := r.store.RenewCollectionLease(ctx, claim, r.options.LeaseDuration); err != nil {
			return fmt.Errorf("renew collection lease before page: %w", err)
		}
		request := event.PageRequest{
			PersonID: claim.PersonID, ForgeInstanceID: claim.ForgeInstanceID,
			Source: source, ScanMode: claim.ScanMode,
			IdentityKeys: claim.IdentityKeys, Window: claim.Window,
			Cursor: claim.Cursor, PageSize: r.options.PageSize,
			AccountNodeID: claim.AccountNodeID, CurrentLogin: claim.CurrentAccountLogin,
			KnownRepositories: claim.KnownRepositories,
		}
		bucket := connector.QuotaBucket()
		if dynamicQuota, ok := connector.(interface {
			QuotaBucketFor(event.PageRequest) string
		}); ok {
			bucket = dynamicQuota.QuotaBucketFor(request)
		}
		if err := request.Validate(); err != nil {
			return r.store.FailCollectionJob(ctx, claim, "invalid collection request")
		}
		var reservationID uuid.UUID
		if bucket != "" {
			reservation, err := r.store.ReserveRateLimit(ctx, bucket, claim, r.options.LeaseDuration)
			if err != nil {
				return fmt.Errorf("reserve upstream quota: %w", err)
			}
			if !reservation.Granted {
				retryAt := time.Now().UTC().Add(r.options.PollInterval)
				if reservation.ResetAt != nil && reservation.ResetAt.After(retryAt) {
					retryAt = *reservation.ResetAt
				}
				return r.store.DeferCollectionJob(ctx, claim, retryAt)
			}
			reservationID = reservation.ID
		}
		page, report, fetchErr := r.fetchPage(ctx, connector, request, claim)
		report = sanitizeFetchReport(report)
		report.Requests++
		report.Pages++
		if report.RateLimitBucket != "" && bucket != "" && report.RateLimitBucket != bucket {
			fetchErr = fmt.Errorf("%w: unexpected rate-limit bucket", ErrInvalidConnectorPage)
		}
		if bucket != "" {
			report.RateLimitBucket = bucket
		}
		if fetchErr != nil {
			report.Errors = append(report.Errors, safeFetchFailure(fetchErr))
			if errors.Is(fetchErr, ErrInvalidConnectorPage) {
				report.Errors = append(report.Errors, "connector returned an invalid page")
				persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, persistErr := r.store.PersistCollectionPage(
					persistCtx, claim, page, report, false, r.options.NormalizerVersion, reservationID)
				cancel()
				if persistErr != nil {
					fetchErr = errors.Join(fetchErr, fmt.Errorf("persist invalid page report: %w", persistErr))
					if reservationID != uuid.Nil {
						releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						err := r.store.ReleaseRateLimit(releaseCtx, reservationID)
						releaseCancel()
						if err != nil {
							return errors.Join(fetchErr, fmt.Errorf("settle quota after invalid page: %w", err))
						}
					}
				}
				failCtx, failCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				failErr := r.store.FailCollectionJob(failCtx, claim, "connector returned an invalid page")
				failCancel()
				return errors.Join(fetchErr, failErr)
			}
			persistCtx := ctx
			cancel := func() {}
			if ctx.Err() != nil {
				persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			}
			_, persistErr := r.store.PersistCollectionPage(
				persistCtx, claim, page, report, false, r.options.NormalizerVersion, reservationID)
			cancel()
			if persistErr != nil && reservationID != uuid.Nil {
				releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				releaseErr := r.store.ReleaseRateLimit(releaseCtx, reservationID)
				releaseCancel()
				if releaseErr != nil {
					persistErr = errors.Join(persistErr, fmt.Errorf("settle quota reservation: %w", releaseErr))
				}
			}
			if errors.Is(fetchErr, context.Canceled) && ctx.Err() != nil {
				return r.releaseCanceledJob(ctx, claim, ctx.Err())
			}
			if persistErr != nil {
				return errors.Join(fmt.Errorf("persist partial collection page: %w", persistErr), fetchErr)
			}
			retryAt := nextRetry(claim.Attempts)
			if report.RateLimitRemaining != nil && *report.RateLimitRemaining == 0 &&
				report.RateLimitResetAt != nil && report.RateLimitResetAt.After(retryAt) {
				retryAt = *report.RateLimitResetAt
			}
			return r.store.RetryCollectionJob(ctx, claim, errors.New(safeFetchFailure(fetchErr)), retryAt)
		}
		done, err := r.store.PersistCollectionPage(
			ctx, claim, page, report, true, r.options.NormalizerVersion, reservationID)
		if err != nil {
			if reservationID != uuid.Nil {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				releaseErr := r.store.ReleaseRateLimit(releaseCtx, reservationID)
				cancel()
				if releaseErr != nil {
					return errors.Join(fmt.Errorf("persist collection page: %w", err),
						fmt.Errorf("settle quota reservation: %w", releaseErr))
				}
			}
			return fmt.Errorf("persist collection page: %w", err)
		}
		if done {
			r.logger.InfoContext(ctx, "collection job completed", "source", source, "job_id", claim.ID)
			return nil
		}
		claim.Cursor = page.NextCursor
	}
}

func (r *Runner) fetchPage(ctx context.Context, connector Connector, request event.PageRequest, claim store.JobClaim) (event.Page, event.FetchReport, error) {
	if err := request.Validate(); err != nil {
		return event.Page{}, event.FetchReport{}, fmt.Errorf("%w: %v", ErrInvalidConnectorPage, err)
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	var heartbeat sync.WaitGroup
	heartbeat.Go(func() {
		ticker := time.NewTicker(leaseHeartbeatInterval(r.options.LeaseDuration))
		defer ticker.Stop()
		for {
			select {
			case <-fetchCtx.Done():
				return
			case <-ticker.C:
				if err := r.store.RenewCollectionLease(fetchCtx, claim, r.options.LeaseDuration); err != nil {
					if errors.Is(err, context.Canceled) && fetchCtx.Err() != nil {
						return
					}
					select {
					case heartbeatErrors <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	})
	page, report, err := connector.FetchPage(fetchCtx, request)
	cancel()
	heartbeat.Wait()
	select {
	case heartbeatErr := <-heartbeatErrors:
		return page, report, fmt.Errorf("maintain collection lease: %w", heartbeatErr)
	default:
	}
	if len(page.Events) > request.PageSize {
		page.Events = validatedEvents(page.Events[:request.PageSize], request)
		return page, report, fmt.Errorf("%w: connector returned more events than the requested page size", ErrInvalidConnectorPage)
	}
	if err != nil {
		page.Events = validatedEvents(page.Events, request)
		return page, report, err
	}
	if !page.Done && (page.NextCursor == "" || page.NextCursor == request.Cursor) {
		page.Events = validatedEvents(page.Events, request)
		return page, report, fmt.Errorf("%w: connector page did not advance its cursor", ErrInvalidConnectorPage)
	}
	if len(validatedEvents(page.Events, request)) != len(page.Events) {
		page.Events = validatedEvents(page.Events, request)
		return page, report, fmt.Errorf("%w: invalid source event", ErrInvalidConnectorPage)
	}
	return page, report, nil
}

func validatedEvents(events []event.RawEvent, request event.PageRequest) []event.RawEvent {
	validated := make([]event.RawEvent, 0, len(events))
	for _, raw := range events {
		if raw.Source != request.Source || raw.Validate(request.ForgeInstanceID) != nil {
			continue
		}
		validated = append(validated, raw)
	}
	return validated
}

func (r *Runner) processNormalization(ctx context.Context, claim store.NormalizationClaim) error {
	normalizationCtx, cancel := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	var heartbeat sync.WaitGroup
	heartbeat.Go(func() {
		ticker := time.NewTicker(leaseHeartbeatInterval(r.options.LeaseDuration))
		defer ticker.Stop()
		for {
			select {
			case <-normalizationCtx.Done():
				return
			case <-ticker.C:
				if err := r.store.RenewNormalizationLease(normalizationCtx, claim, r.options.LeaseDuration); err != nil {
					if errors.Is(err, context.Canceled) && normalizationCtx.Err() != nil {
						return
					}
					select {
					case heartbeatErrors <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	})
	actions, normalizeErr := r.normalizer.Normalize(claim.Event)
	cancel()
	heartbeat.Wait()
	select {
	case heartbeatErr := <-heartbeatErrors:
		return fmt.Errorf("maintain normalization lease: %w", heartbeatErr)
	default:
	}
	if normalizeErr != nil {
		retryAt := nextRetry(claim.Attempts)
		if retryErr := r.store.RetryNormalizationJob(ctx, claim, errors.New("normalization failed"), retryAt); retryErr != nil {
			return errors.Join(fmt.Errorf("normalize raw event: %w", normalizeErr), retryErr)
		}
		r.logger.ErrorContext(ctx, "normalization will retry", "raw_event_id", claim.RawEventID,
			"normalizer_version", claim.NormalizerVersion, "err", safeFetchFailure(normalizeErr))
		return nil
	}
	if err := r.store.CompleteNormalization(ctx, claim, actions); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			return err
		}
		retryAt := nextRetry(claim.Attempts)
		if retryErr := r.store.RetryNormalizationJob(ctx, claim, errors.New("normalization persistence failed"), retryAt); retryErr != nil {
			return errors.Join(fmt.Errorf("persist normalized event: %w", err), retryErr)
		}
		r.logger.ErrorContext(ctx, "normalization persistence will retry",
			"raw_event_id", claim.RawEventID, "err", safeFetchFailure(err))
	}
	return nil
}

func (r *Runner) releaseCanceledJob(ctx context.Context, claim store.JobClaim, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.store.ReleaseCollectionJob(cleanupCtx, claim); err != nil {
		return errors.Join(cause, fmt.Errorf("release canceled collection job: %w", err))
	}
	return cause
}

func nextRetry(attempt int) time.Time {
	if attempt < 1 {
		attempt = 1
	}
	exponent := min(attempt-1, 10)
	delay := 5 * time.Second * time.Duration(1<<exponent)
	delay = min(delay, time.Hour)
	jitter := time.Duration(rand.Int64N(delay.Microseconds())) * time.Microsecond
	return time.Now().UTC().Add(delay/2 + jitter)
}

func leaseHeartbeatInterval(lease time.Duration) time.Duration {
	return max(lease/3, time.Millisecond)
}

func safeFetchFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "operation canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation timed out"
	default:
		return "upstream or normalization operation failed"
	}
}

func sanitizeFetchReport(report event.FetchReport) event.FetchReport {
	for i := range report.Errors {
		report.Errors[i] = "source reported an error"
	}
	return report
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
