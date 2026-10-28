package workshops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-atelier/internal/platform/httpx"
)

const (
	maxSeatsTotal      = 5000
	maxPriceCents      = 1_000_000
	defaultPerAttendee = 2
)

// Workshop is the public view of a workshop.
type Workshop struct {
	ID             uuid.UUID `json:"id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	Track          string    `json:"track"`
	Instructor     string    `json:"instructor"`
	WorkshopDate   time.Time `json:"workshopDate"`
	PriceCents     int64     `json:"priceCents"`
	SeatsTotal     int       `json:"seatsTotal"`
	SeatsLeft      int       `json:"seatsLeft"`
	MaxPerAttendee int       `json:"maxPerAttendee"`
	KitsTotal      int       `json:"kitsTotal"`
	KitsLeft       int       `json:"kitsLeft"`
	KitPriceCents  int64     `json:"kitPriceCents"`
}

// Info is what other modules may ask the workshops module (Orders never reads this schema).
type Info struct {
	ID            uuid.UUID
	Code          string
	PriceCents    int64
	KitPriceCents int64
	KitsTotal     int
}

// Catalog answers the one cross-module read: a workshop's existence and price.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a workshop up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code, price_cents, kit_price_cents, kits_total FROM workshops.workshops WHERE code = $1`, code).Scan(&i.ID, &i.Code, &i.PriceCents, &i.KitPriceCents, &i.KitsTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the workshops HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the workshop endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/workshops", authed(a.list))
	mux.HandleFunc("GET /api/workshops/{code}", authed(a.get))
	mux.HandleFunc("POST /api/workshops", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const workshopSelect = `SELECT id, code, name, track, instructor, workshop_date, price_cents, seats_total, seats_left, max_per_attendee, kits_total, kits_left, kit_price_cents FROM workshops.workshops`

func scanWorkshop(row pgx.Row) (Workshop, error) {
	var d Workshop
	err := row.Scan(&d.ID, &d.Code, &d.Name, &d.Track, &d.Instructor, &d.WorkshopDate, &d.PriceCents, &d.SeatsTotal, &d.SeatsLeft, &d.MaxPerAttendee, &d.KitsTotal, &d.KitsLeft, &d.KitPriceCents)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), workshopSelect+` ORDER BY workshop_date, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Workshop{}
	for rows.Next() {
		d, err := scanWorkshop(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanWorkshop(a.Pool.QueryRow(r.Context(), workshopSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	Track          string     `json:"track"`
	Instructor     string     `json:"instructor"`
	WorkshopDate   *time.Time `json:"workshopDate"`
	SeatsTotal     int64      `json:"seatsTotal"`
	PriceCents     int64      `json:"priceCents"`
	MaxPerAttendee *int64     `json:"maxPerAttendee"`
	KitsTotal      *int64     `json:"kitsTotal"`
	KitPriceCents  *int64     `json:"kitPriceCents"`
}

// create opens a workshop. Staff only. This is the only place seats_left is initialised; after that only the reserve and
// release handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Track) == "" || strings.TrimSpace(req.Instructor) == "" {
		return httpx.Fail(http.StatusBadRequest, "Name, track and instructor are required.")
	}
	if req.SeatsTotal < 1 || req.SeatsTotal > maxSeatsTotal {
		return httpx.Fail(http.StatusBadRequest, "Seats on sale must be between 1 and %d seats.", maxSeatsTotal)
	}
	if req.PriceCents < 1 || req.PriceCents > maxPriceCents {
		return httpx.Fail(http.StatusBadRequest, "Price must be between 1 and %d cents per seat.", maxPriceCents)
	}
	limit := int64(defaultPerAttendee)
	if req.MaxPerAttendee != nil {
		limit = *req.MaxPerAttendee
	}
	if limit < 1 || limit > req.SeatsTotal {
		return httpx.Fail(http.StatusBadRequest, "Per-attendee cap must be at least 1 and no more than the seats on sale.")
	}
	var kits, kitPrice int64
	if req.KitsTotal != nil {
		kits = *req.KitsTotal
	}
	if req.KitPriceCents != nil {
		kitPrice = *req.KitPriceCents
	}
	if kits < 0 || kits > maxSeatsTotal {
		return httpx.Fail(http.StatusBadRequest, "Kits must be between 0 and %d.", maxSeatsTotal)
	}
	if kitPrice < 0 || kitPrice > maxPriceCents || (kits > 0 && kitPrice < 1) {
		return httpx.Fail(http.StatusBadRequest, "A kit needs a price between 1 and the seat price limit when kits are offered.")
	}
	release := time.Now().AddDate(0, 0, 14)
	if req.WorkshopDate != nil {
		release = *req.WorkshopDate
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO workshops.workshops (id, code, name, track, instructor, workshop_date, price_cents, seats_total, seats_left, max_per_attendee, kits_total, kits_left, kit_price_cents, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10,$10,$11,now())`,
		id, code, strings.TrimSpace(req.Name), strings.TrimSpace(req.Track), strings.TrimSpace(req.Instructor), release, req.PriceCents, req.SeatsTotal, limit, kits, kitPrice)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Workshop '%s' already exists.", code)
		}
		return err
	}
	d, err := scanWorkshop(a.Pool.QueryRow(r.Context(), workshopSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/workshops/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}
