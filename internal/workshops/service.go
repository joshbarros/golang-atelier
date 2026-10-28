package workshops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-atelier/internal/platform/messaging"
	"github.com/joshbarros/golang-atelier/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "workshops"
	ConsumerName = "workshops"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	CapReached
	Duplicate
)

// Service is the workshops module's logic. It is the only writer of seats_left and of the per-attendee counts.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "workshops"}, Log: log}
}

// Handle implements messaging.Handler for the workshops queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("workshops does not handle %q", d.RoutingKey)
}

var errRefused = errors.New("the cap or the stock said no")

// Reserve takes seats from a workshop in two atomic steps inside one transaction: a per-attendee cap upsert that only moves
// while the new total still fits, then one conditional UPDATE that matches only while the remaining stock covers the
// request. If either says no, the whole attempt rolls back (so a cap increment never outlives a failed reserve) and the
// refusal is recorded in a fresh transaction.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveSeats) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		// 1. the per-attendee cap: one atomic upsert that only moves while the new total still fits
		capTag, err := tx.Exec(ctx, `
			INSERT INTO workshops.attendees (workshop_id, owner_id, seats)
			SELECT d.id, $2, $3 FROM workshops.workshops d WHERE d.id = $1 AND d.max_per_attendee >= $3
			ON CONFLICT (workshop_id, owner_id) DO UPDATE SET seats = workshops.attendees.seats + $3
			WHERE workshops.attendees.seats + $3 <= (SELECT max_per_attendee FROM workshops.workshops WHERE id = $1)`,
			cmd.WorkshopID, cmd.OwnerID, cmd.Seats)
		if err != nil {
			return err
		}
		if capTag.RowsAffected() == 1 {
			// 2. the remaining seats: one conditional UPDATE, the only writer of seats_left
			// One conditional UPDATE over both counters: the seats and the kits move together or not at all. If the kits are gone
			// the seats are not touched either.
			kits := 0
			if cmd.Kit {
				kits = cmd.Seats
			}
			stockTag, err := tx.Exec(ctx, `
				UPDATE workshops.workshops SET seats_left = seats_left - $2, kits_left = kits_left - $3
				WHERE id = $1 AND seats_left >= $2 AND kits_left >= $3`, cmd.WorkshopID, cmd.Seats, kits)
			if err != nil {
				return err
			}
			if stockTag.RowsAffected() == 1 {
				if _, err := tx.Exec(ctx, `INSERT INTO workshops.seat_holds (order_id, owner_id, workshop_id, seats, kits, status, created_at) VALUES ($1,$2,$3,$4,$5,'Held',now())`,
					cmd.OrderID, cmd.OwnerID, cmd.WorkshopID, cmd.Seats, kits); err != nil {
					return err
				}
				return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.OrderID,
					messaging.SeatsHeld{OrderID: cmd.OrderID, OwnerID: cmd.OwnerID, WorkshopID: cmd.WorkshopID, Seats: cmd.Seats, Reference: cmd.Reference})
			}
		}
		return errRefused // roll the cap increment back with everything else
	})
	if err == nil {
		return outcome, nil
	}
	if !errors.Is(err, errRefused) {
		return 0, err
	}
	// The refusal is recorded in a fresh transaction (the inbox claim was rolled back with the attempt).
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		var left, limit, kitsLeft int
		_ = tx.QueryRow(ctx, `SELECT seats_left, max_per_attendee, kits_left FROM workshops.workshops WHERE id = $1`, cmd.WorkshopID).Scan(&left, &limit, &kitsLeft)
		var holding int
		_ = tx.QueryRow(ctx, `SELECT seats FROM workshops.attendees WHERE workshop_id = $1 AND owner_id = $2`, cmd.WorkshopID, cmd.OwnerID).Scan(&holding)
		reason := "stock"
		outcome = Short
		if limit > 0 && holding+cmd.Seats > limit {
			reason = "cap"
			outcome = CapReached
		} else if cmd.Kit && kitsLeft < cmd.Seats {
			reason = "kits" // the seats were there, the kits were not: nothing was taken
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
			messaging.SeatsShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedSeats: cmd.Seats, SeatsLeft: left, Reason: reason, PerAttendeeLimit: limit, KitsLeft: kitsLeft})
	})
	return outcome, err
}

// Release gives held seats back exactly once, returning the attendee's allowance too. A second release changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseSeats) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var workshopID, ownerID uuid.UUID
		var seats, kits int
		err = tx.QueryRow(ctx, `
			UPDATE workshops.seat_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE order_id = $1 AND status = 'Held' RETURNING workshop_id, owner_id, seats, kits`, cmd.OrderID, cmd.Reason).Scan(&workshopID, &ownerID, &seats, &kits)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workshops.workshops SET seats_left = seats_left + $2, kits_left = kits_left + $3 WHERE id = $1 AND seats_left + $2 <= seats_total AND kits_left + $3 <= kits_total`, workshopID, seats, kits); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workshops.attendees SET seats = seats - $3 WHERE workshop_id = $1 AND owner_id = $2 AND seats >= $3`, workshopID, ownerID, seats); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.SeatsReleased{OrderID: cmd.OrderID, Seats: seats, Reason: cmd.Reason})
	})
	return released, err
}

var _ = time.Second
