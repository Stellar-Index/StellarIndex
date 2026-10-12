package middleware

import "net/http"

// SecurityHeaders sets nosniff and Referrer-Policy no-referrer (some API URLs carry credentials in the query); HSTS and CSP stay at the edge, which must set Referrer-Policy set-if-absent (`?Referrer-Policy`) or it overwrites no-referrer.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
