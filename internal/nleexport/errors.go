package nleexport

import "errors"

// ErrInvalidTimeline reports that a timeline cannot be serialized as an NLE
// interchange document. Every error this package returns wraps it, so a caller
// classifies the whole refusal with errors.Is instead of matching message
// text. The boundary exists because the API used to recognize a refused
// export by the literal prefix "nleexport: " on the message, and rewording an
// error message — ordinary maintenance — silently reclassified a refusal as a
// Hub 500 that an operator would read as a bug and retry. The sentinel is
// deliberately broad: a mixed frame rate, a shot past the end of its file, and
// a caller that forgot to supply a source all mean "this cannot be written",
// and the distinction that matters to an operator belongs in the wrapped
// message, not in the sentinel.
var ErrInvalidTimeline = errors.New("nleexport: invalid timeline")
