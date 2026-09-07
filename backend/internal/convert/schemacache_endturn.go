package convert

// injectEndTurnTool appends the end_turn tool definition to pass Codebuff foreign_toolset validation.
// An existing end_turn is never duplicated. Moved verbatim from normalizeToolSchemas.
func injectEndTurnTool(payload map[string]any, tools []any, hasEndTurn bool) {
	if !hasEndTurn {
		payload["tools"] = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "end_turn",
				"description": "Signal the end of the current task.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		})
	}
}
