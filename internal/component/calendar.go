package component

// Routes of the Google Calendar connection flow. The profile page links to
// the connect path and posts a form to the disconnect path; Google redirects
// back to the callback path.
const (
	GoogleCalendarConnectPath    = "/auth/google/calendar/connect"
	GoogleCalendarCallbackPath   = "/auth/google/calendar/callback"
	GoogleCalendarDisconnectPath = "/auth/google/calendar/disconnect"
)

// CalendarState is what the profile page needs to render its Google Calendar
// section. ConnectFailed is only a flag: the page shows fixed copy, never any
// text taken from the request.
type CalendarState struct {
	// GoogleAvailable is true when Google credentials are configured.
	GoogleAvailable bool
	// Connected is true when the user has a stored Google Calendar connection.
	Connected bool
	// Email is the Google account email of the stored connection.
	Email string
	// ConnectFailed is true when the last connect attempt failed.
	ConnectFailed bool
}
