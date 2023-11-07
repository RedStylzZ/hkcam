package api

import (
	"net/http"

	"github.com/RedStylzZ/hkcam/api/apiutil"
)

// WriteJSON responds to request r by encoding and sending v as json.
// If v is an instance of an ErrResponse, the response status code is 400 (Bad Request).
func WriteJSON(w http.ResponseWriter, r *http.Request, v interface{}) error {
	switch v.(type) {
	case *ErrResponse, ErrResponse:
		w.WriteHeader(http.StatusBadRequest)
	default:
		w.WriteHeader(http.StatusOK)
	}

	return apiutil.WriteJSON(w, r, v)
}
