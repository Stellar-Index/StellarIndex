// Package middleware has the HTTP middleware the v1 Server wraps its mux
// in, composed by [Chain] in declaration order. Outermost first, with
// every optional entry wired:
//
//	RequestID → HTTPMetrics → Logger → Recoverer → SecurityHeaders →
//	CacheControl → Envelope404 → CORS → TrailingSlashRedirect →
//	ResolveRoute → RequestTimeout → PublicRoutes → Auth → KeyPolicy →
//	RequireEmailVerified → UsageTracker → MonthlyQuota → RateLimit →
//	TouchUsage → SessionAuth → ETag → CaptureRoute
//
// CORS through SessionAuth, except the always-on ETag and CaptureRoute,
// are wired only when configured. TestMiddlewareStackMatchesPackageDoc
// fails when this list and Server.middlewareStack disagree.
//
// [Auth] picks the subject per `[api].auth_mode` (anonymous, apikey,
// sep10) and stamps it on the context; handlers read context values only
// through accessors such as [auth.SubjectFrom]. CORS is an exact-match
// allow-list plus wildcard, nothing more.
package middleware
