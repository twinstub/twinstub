package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// hint is an error message written for the agent: full sentences that end
// with what to do next.
type hint string

func (h hint) Error() string { return string(h) }

// explain turns an error into one message that says what went wrong and
// what to call next. The agent sees only this text.
func (s *Server) explain(err error) error {
	var ue *unreachableError
	if errors.As(err, &ue) {
		switch {
		case s.explicit:
			return hint(fmt.Sprintf("Engine not reachable at %s (set via --admin-url or TWINSTUB_ADMIN_URL). Start it with `twinstub serve` or remove the setting to use the embedded engine.", ue.url))
		case s.mode == ModeExternal:
			return hint(fmt.Sprintf("The engine at %s stopped answering. Start it again with `twinstub serve`, or restart the MCP server to run an embedded engine.", ue.url))
		default:
			return hint(fmt.Sprintf("The embedded engine is not answering (%v). Restart the MCP server.", ue.err))
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return hint("The engine did not answer in time. The app may be slow to answer webhooks; check it, or start twinstub mcp with a longer --timeout.")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	msg := strings.TrimSuffix(ae.Error(), ".")
	if next := hintFor(ae); next != "" {
		msg += ". " + next
	}
	return hint(msg)
}

func hintFor(e *apiError) string {
	switch e.code {
	case "unauthorized":
		return "The engine requires an admin token: set --token or TWINSTUB_TOKEN."
	case "session_not_found":
		return "Start one with twinstub_start_session; sessions also end when their TTL expires or on twinstub_reset."
	case "scenario_not_found", "unknown_scenario":
		return "Known scenarios: " + list(e.body["known_scenarios"]) + ". twinstub_list_scenarios describes them."
	case "unknown_state":
		return "States of this scenario: " + list(e.body["states"]) + "."
	case "advance_failed":
		return "twinstub_get_session shows the current state; twinstub_list_scenarios lists the states."
	case "delivery_not_found":
		return "twinstub_list_deliveries lists delivery ids; the journal keeps only the most recent deliveries."
	case "not_pending":
		return "Queued events: " + list(e.body["pending_events"]) + ". An earlier item may queue it, or the app may need to make its request first. To resend a delivered event use twinstub_replay_delivery."
	case "session_busy":
		return "Another advance_time or deliver call on this session is still running; wait for it to finish."
	case "target_not_found":
		return "Add the target to the config, then call twinstub_load_config."
	case "reload_failed":
		return "twinstub_validate_config shows every error in the file."
	case "cursor_expired":
		return "Call twinstub_list_deliveries again without a cursor."
	case "bad_mode":
		return "Use as_is, redeliver, duplicate, bad_signature or no_signature."
	}
	return ""
}

func list(v any) string {
	items, _ := v.([]any)
	if len(items) == 0 {
		return "none"
	}
	parts := make([]string, len(items))
	for i, x := range items {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ", ")
}
