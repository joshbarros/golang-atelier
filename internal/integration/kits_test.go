package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-atelier/internal/platform/messaging"
	"github.com/joshbarros/golang-atelier/internal/workshops"
)

// openKitWorkshop opens a workshop with seats and kits (a kit costs 1500 cents) through the API; the cap is `cap` seats.
func (e *env) openKitWorkshop(seats, kits, cap int) string {
	e.t.Helper()
	c := code()
	r := e.post("/api/workshops", map[string]any{
		"code": c, "name": "Test Workshop", "track": "Backend", "instructor": "Dr. Marlo", "priceCents": 5000, "seatsTotal": seats,
		"maxPerAttendee": cap, "kitsTotal": kits, "kitPriceCents": 1500,
	}, e.admin().Token)
	if r.Status != 201 {
		e.t.Fatalf("open workshop: %d %s", r.Status, r.Raw)
	}
	return c
}

func (e *env) bundle(s session, workshop, reference string, seats int, kit bool) reply {
	return e.post("/api/orders", map[string]any{"workshopCode": workshop, "reference": reference, "seats": seats, "kit": kit}, s.Token)
}

func (e *env) kitsLeft(workshop string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/workshops/"+workshop, e.admin().Token).Body["kitsLeft"].(float64)
	return v
}

func TestTwelveConcurrentBundlesAgainstThreeKitsSellThreeAndTheRefusedNineTakeNoSeat(t *testing.T) {
	e := newEnv(t)
	workshop := e.openKitWorkshop(30, 3, 4)
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM workshops.workshops WHERE code = $1`, workshop).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[workshops.ReserveOutcome]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := messaging.ReserveSeats{OrderID: uuid.New(), OwnerID: uuid.New(), WorkshopID: id, Seats: 1, Reference: "R", Kit: true}
			out, err := e.app.Workshops.Reserve(context.Background(), uuid.New(), cmd)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			outcomes[out]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	var seatsLeft, kitsLeft int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left, kits_left FROM workshops.workshops WHERE id = $1`, id).Scan(&seatsLeft, &kitsLeft)
	if outcomes[workshops.Held] != 3 || outcomes[workshops.Short] != 9 || seatsLeft != 27 || kitsLeft != 0 {
		t.Fatalf("three bundles sell, nine are refused and take no seat: %v, %d seats and %d kits left", outcomes, seatsLeft, kitsLeft)
	}
}

func TestWhenTheKitsRunOutTheSeatsStayOnTheShelfForPeopleWhoDoNotNeedOne(t *testing.T) {
	e := newEnv(t)
	workshop := e.openKitWorkshop(10, 1, 4)
	ana, bob, cy := e.register(), e.register(), e.register()
	a := e.bundle(ana, workshop, "ATT-A", 1, true).str("id")
	e.drain()
	b := e.bundle(bob, workshop, "ATT-B", 2, true).str("id")
	e.drain()
	c := e.bundle(cy, workshop, "ATT-C", 2, false).str("id")
	e.drain()
	if status(e, ana, a) != "Sold" {
		t.Fatal("the first bundle sells")
	}
	if o := e.order(bob, b); o.str("status") != "Rejected" || o.str("reason") != "Kits: 2 kits wanted, 0 left. No seat was taken." {
		t.Fatalf("the second bundle is refused whole: %s", o.Raw)
	}
	if status(e, cy, c) != "Sold" {
		t.Fatal("a booking without a kit still sells")
	}
	if e.seatsLeft(workshop) != 7 || e.kitsLeft(workshop) != 0 {
		t.Fatalf("1 + 2 seats taken, the refused bundle took none: %v seats, %v kits", e.seatsLeft(workshop), e.kitsLeft(workshop))
	}
}

func TestWhenTheSeatsRunOutTheKitsStayToo(t *testing.T) {
	e := newEnv(t)
	workshop := e.openKitWorkshop(2, 5, 2)
	ana, bob := e.register(), e.register()
	e.bundle(ana, workshop, "ATT-A", 2, true)
	e.drain()
	b := e.bundle(bob, workshop, "ATT-B", 1, true).str("id")
	e.drain()
	if o := e.order(bob, b); o.str("status") != "Rejected" || o.str("reason") != "Short: 1 seats wanted, 0 left." {
		t.Fatalf("no seat left: %s", o.Raw)
	}
	if e.kitsLeft(workshop) != 3 {
		t.Fatalf("the refused bundle took no kit: %v", e.kitsLeft(workshop))
	}
}

func TestAReleaseReturnsSeatsAndKitsTogetherAndTheTotalCountsTheKit(t *testing.T) {
	e := newEnv(t)
	workshop := e.openKitWorkshop(10, 4, 4)
	ana := e.register()
	created := e.bundle(ana, workshop, "ATT-A", 2, true)
	if created.Body["totalCents"] != float64(14560) || created.Body["kit"] != true { // 2 x (5000 + 1500) + 12%
		t.Fatalf("the total counts the kit price for every seat: %s", created.Raw)
	}
	id := created.str("id")
	e.drain()
	if e.seatsLeft(workshop) != 8 || e.kitsLeft(workshop) != 2 {
		t.Fatal("two seats and two kits held")
	}
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if e.seatsLeft(workshop) != 10 || e.kitsLeft(workshop) != 4 {
		t.Fatalf("seats and kits come back together: %v / %v", e.seatsLeft(workshop), e.kitsLeft(workshop))
	}
	if status(e, ana, id) != "Refunded" {
		t.Fatal("refunded")
	}
}

func TestAKitAskedOfAWorkshopWithoutKitsIsRefusedAtTheDoor(t *testing.T) {
	e := newEnv(t)
	workshop := e.openKitWorkshop(5, 0, 2)
	r := e.bundle(e.register(), workshop, "ATT-A", 1, true)
	if r.Status != 400 || r.str("title") != "Workshop '"+workshop+"' has no kits to lend." {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if r := e.bundle(e.register(), workshop, "ATT-B", 1, false); r.Status != 202 {
		t.Fatalf("a plain booking is fine: %d", r.Status)
	}
}

func TestOpeningAWorkshopValidatesItsKits(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	base := func() map[string]any {
		return map[string]any{"code": code(), "name": "n", "track": "t", "instructor": "i", "priceCents": 100, "seatsTotal": 10}
	}
	for name, mut := range map[string]func(map[string]any){
		"negative kits":        func(m map[string]any) { m["kitsTotal"] = -1; m["kitPriceCents"] = 100 },
		"too many kits":        func(m map[string]any) { m["kitsTotal"] = 5001; m["kitPriceCents"] = 100 },
		"kits without a price": func(m map[string]any) { m["kitsTotal"] = 3 },
		"a price too high":     func(m map[string]any) { m["kitsTotal"] = 3; m["kitPriceCents"] = 1_000_001 },
	} {
		m := base()
		mut(m)
		if r := e.post("/api/workshops", m, adm.Token); r.Status != 400 {
			t.Errorf("%s: %d", name, r.Status)
		}
	}
	ok := base()
	ok["kitsTotal"] = 3
	ok["kitPriceCents"] = 900
	if r := e.post("/api/workshops", ok, adm.Token); r.Status != 201 || r.Body["kitsLeft"] != float64(3) || r.Body["kitPriceCents"] != float64(900) {
		t.Fatalf("a workshop with kits: %d %s", r.Status, r.Raw)
	}
}
