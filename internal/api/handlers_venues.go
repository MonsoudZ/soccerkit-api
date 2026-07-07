package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

func (s *Server) handleListVenues(w http.ResponseWriter, r *http.Request) {
	venues, err := s.store.ListVenues(r.Context(), store.ListVenuesParams{
		City: queryStrPtr(r, "city"),
		Lim:  queryInt(r, "limit", 50, 1, 100),
		Off:  queryInt(r, "offset", 0, 0, 1_000_000),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Venue, len(venues))
	for i, v := range venues {
		out[i] = venueDTO(v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetVenue(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	v, err := s.store.GetVenue(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("venue not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, venueDTO(v))
}

type createVenueRequest struct {
	Name      string   `json:"name"`
	Address   *string  `json:"address"`
	City      *string  `json:"city"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Surface   *string  `json:"surface"`
}

var validSurfaces = map[string]bool{"GRASS": true, "TURF": true, "INDOOR": true, "CONCRETE": true}

func (s *Server) handleCreateVenue(w http.ResponseWriter, r *http.Request) {
	var req createVenueRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, errValidation("name is required"))
		return
	}
	if req.Surface != nil && !validSurfaces[*req.Surface] {
		writeError(w, errValidation("surface must be one of GRASS, TURF, INDOOR, CONCRETE"))
		return
	}
	v, err := s.store.CreateVenue(r.Context(), store.CreateVenueParams{
		Name: req.Name, Address: req.Address, City: req.City,
		Latitude: req.Latitude, Longitude: req.Longitude, Surface: req.Surface,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, venueDTO(v))
}
