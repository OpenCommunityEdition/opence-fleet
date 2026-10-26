package web

import (
	"net/http"
	"net/url"

	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
)

func urlEscape(text string) string { return url.QueryEscape(text) }

func urlUnescape(text string) (string, error) { return url.QueryUnescape(text) }

func writeJSON(w http.ResponseWriter, value any) { httpx.JSON(w, http.StatusOK, value) }
