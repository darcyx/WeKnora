package errors

import "errors"

var (
	// ErrMessageFeedbackAlreadySubmitted is returned when a message already
	// carries a like/dislike vote. Feedback is one-shot: once recorded it can
	// never be resubmitted or edited.
	ErrMessageFeedbackAlreadySubmitted = errors.New("message feedback already submitted")
)
