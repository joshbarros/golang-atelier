package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-atelier/internal/platform/messaging"
	"github.com/joshbarros/golang-atelier/internal/workshops"
)

// rawWorkshop inserts a workshop directly (below the API) and returns its id.
func (e *env) rawWorkshop(seats, perAttendee int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO workshops.workshops (id, code, name, track, instructor, workshop_date, price_cents, seats_total, seats_left, max_per_attendee, created_at)
		VALUES ($1,$2,'n','s','a',now(),5000,$3,$3,$4,now())`, id, code(), seats, perAttendee)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, workshop, owner uuid.UUID, seats int) workshops.ReserveOutcome {
	out, err := e.app.Workshops.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{OrderID: uuid.New(), OwnerID: owner, WorkshopID: workshop, Seats: seats, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	workshop := e.rawWorkshop(30, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, workshop, uuid.New(), 1) == workshops.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM workshops.workshops WHERE id = $1`, workshop).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 attendees, 30 seats: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM workshops.seat_holds WHERE workshop_id = $1`, workshop) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestTheSameAttendeeRacingThemselvesCannotPassTheCap(t *testing.T) {
	e := newEnv(t)
	workshop := e.rawWorkshop(50, 2)
	owner := uuid.New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[workshops.ReserveOutcome]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := reserve(e, workshop, owner, 1)
			mu.Lock()
			outcomes[o]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if outcomes[workshops.Held] != 2 || outcomes[workshops.CapReached] != 10 {
		t.Fatalf("a cap of 2 lets exactly 2 through: %v", outcomes)
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM workshops.workshops WHERE id = $1`, workshop).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT seats FROM workshops.attendees WHERE workshop_id = $1 AND owner_id = $2`, workshop, owner).Scan(&holding)
	if left != 48 || holding != 2 {
		t.Fatalf("refused tries leave no trace: %d left, attendee holds %d", left, holding)
	}
}

func TestACapRefusalDoesNotConsumeStockAndAStockRefusalDoesNotConsumeAllowance(t *testing.T) {
	e := newEnv(t)
	workshop := e.rawWorkshop(3, 2)
	owner := uuid.New()
	if reserve(e, workshop, owner, 3) != workshops.CapReached {
		t.Fatal("asking for more than the cap in one go is a cap refusal")
	}
	other := uuid.New()
	if reserve(e, workshop, other, 2) != workshops.Held {
		t.Fatal("2 of 3 held")
	}
	if reserve(e, workshop, owner, 2) != workshops.Short {
		t.Fatal("1 left, 2 wanted, within the cap: short")
	}
	if reserve(e, workshop, owner, 1) != workshops.Held {
		t.Fatal("the allowance was not spent by the short try")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM workshops.workshops WHERE id = $1`, workshop).Scan(&left)
	if left != 0 {
		t.Fatalf("all three are held: %d", left)
	}
}

func TestReleaseGivesBackStockAndAllowanceExactlyOnce(t *testing.T) {
	e := newEnv(t)
	workshop := e.rawWorkshop(5, 2)
	owner, order := uuid.New(), uuid.New()
	if out, _ := e.app.Workshops.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{OrderID: order, OwnerID: owner, WorkshopID: workshop, Seats: 2, Reference: "R"}); out != workshops.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		_, err := e.app.Workshops.Release(context.Background(), uuid.New(), messaging.ReleaseSeats{OrderID: order, Reason: "refund"})
		if err != nil {
			t.Fatal(err)
		}
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM workshops.workshops WHERE id = $1`, workshop).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT seats FROM workshops.attendees WHERE workshop_id = $1 AND owner_id = $2`, workshop, owner).Scan(&holding)
	if left != 5 || holding != 0 {
		t.Fatalf("given back once, not three times: %d left, attendee holds %d", left, holding)
	}
	if e.outbox("workshops", messaging.ReleasedKey) != 1 {
		t.Fatal("one released event")
	}
}

func TestOpeningAWorkshopValidatesItsCap(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := map[string]any{"code": code(), "name": "n", "track": "s", "instructor": "a", "priceCents": 100, "seatsTotal": 10}
	if r := e.post("/api/workshops", good, adm.Token); r.Status != 201 || r.Body["maxPerAttendee"] != float64(2) {
		t.Fatalf("the cap defaults to 2: %d %s", r.Status, r.Raw)
	}
	bad := map[string]any{"code": code(), "name": "n", "track": "s", "instructor": "a", "priceCents": 100, "seatsTotal": 10, "maxPerAttendee": 11}
	if r := e.post("/api/workshops", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap above the seats on sale: %d", r.Status)
	}
	bad["maxPerAttendee"] = 0
	if r := e.post("/api/workshops", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap of zero: %d", r.Status)
	}
	if r := e.post("/api/workshops", good, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/workshops", good, e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a workshop: %d", r.Status)
	}
}
