package agp

import (
	"github.com/google/uuid"
)

const (
	MethodRoute       = "agp/route"
	MethodRouteIntent = "agp/route_intent"
	MethodRouteTask   = "agp/route_task"
)

// ForwardingMethods is what the gateway card advertises under the AGP
// extension's "forwarding" parameter, so a gateway that reads the card knows
// which RPC methods this marketplace serves for handing an intent to an agent.
var ForwardingMethods = []string{MethodRouteIntent, MethodRouteTask}

// TaskMessage is the A2A task message a caller sends to the selected agent.
// The payload is the caller's own intent content wrapped in routing metadata;
// the marketplace renders the envelope, the caller owns the content.
type TaskMessage struct {
	Role             string         `json:"role"`
	Kind             string         `json:"kind"`
	TaskID           string         `json:"taskId"`
	ContextID        string         `json:"contextId"`
	TargetCapability string         `json:"targetCapability,omitempty"`
	Payload          map[string]any `json:"payload"`
}

type TaskParams struct {
	Message TaskMessage `json:"message"`
}

// TaskEnvelope is a single JSON-RPC message body a caller can POST to the
// selected squad path without building the A2A envelope from scratch.
type TaskEnvelope struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      string     `json:"id"`
	Method  string     `json:"method"`
	Params  TaskParams `json:"params"`
}

// RouteTask behaves exactly like Route but also renders task: a JSON-RPC
// "message" envelope addressed to the selected route. Rendering is read-only —
// nothing on the service side is invoked by building it — so route_task is
// Route plus metadata over caller-supplied content, and the failure modes
// (route not found, policy violation, stale table) are Route's.
func (t Table) RouteTask(intent Intent) (RouteResult, error) {
	result, err := t.Route(intent)
	if err != nil {
		return RouteResult{}, err
	}
	task := RenderTask(intent, result.Route)
	result.Task = &task
	return result, nil
}

func RenderTask(intent Intent, route RouteEntry) TaskEnvelope {
	return TaskEnvelope{
		JSONRPC: "2.0",
		ID:      uuid.NewString(),
		Method:  "message",
		Params: TaskParams{
			Message: TaskMessage{
				Role:             "agent",
				Kind:             "task",
				TaskID:           uuid.NewString(),
				ContextID:        uuid.NewString(),
				TargetCapability: NormalizeCapability(intent.TargetCapability),
				Payload:          intent.Payload,
			},
		},
	}
}
