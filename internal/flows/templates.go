package flows

// Template is a built-in flow to start from (GET /api/flows/templates).
// Flow has no id and version 0; models are "default" and the agent is
// the built-in "cerberus" profile, for the owner to change.
type Template struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Flow        Flow   `json:"flow"`
}

// Templates are the built-in templates: Clarify → Answer, Draft →
// Critique → Revise, Plan → Delegate → Merge and Website. Each call
// answers fresh copies.
func Templates() []Template {
	manual := Trigger{Kind: TriggerManual}
	return []Template{
		{
			Key: "clarify-answer", Name: "Clarify → Answer",
			Description: "Restate the question precisely, then answer the clarified question.",
			Flow: Flow{Name: "Clarify → Answer", Trigger: manual, InputHint: "A question",
				Description: "Restates the question precisely, then answers it.",
				Steps: []Step{
					{ID: "clarify", Kind: KindPrompt, Name: "Clarify", Model: "default",
						Template: "Restate this question so it is precise and self-contained. List any assumptions you make. Do not answer it.\n\n{input}"},
					{ID: "answer", Kind: KindPrompt, Name: "Answer", Model: "default",
						Template: "Answer the question below. Be accurate and concise.\n\n{steps.clarify.output}"},
					{ID: "notify", Kind: KindOutput, Name: "Notify", To: ToInbox, Template: "{steps.answer.output}"},
				}},
		},
		{
			Key: "draft-critique-revise", Name: "Draft → Critique → Revise",
			Description: "Write a draft, critique it, then revise it with the critique.",
			Flow: Flow{Name: "Draft → Critique → Revise", Trigger: manual, InputHint: "What to write",
				Description: "Drafts, critiques and revises a piece of writing.",
				Steps: []Step{
					{ID: "draft", Kind: KindPrompt, Name: "Draft", Model: "default", Template: "Write: {input}"},
					{ID: "critique", Kind: KindPrompt, Name: "Critique", Model: "default",
						Template: "Critique this draft: what is unclear, wrong or missing? Be specific and short.\n\nTask: {input}\n\nDraft:\n{steps.draft.output}"},
					{ID: "revise", Kind: KindPrompt, Name: "Revise", Model: "default",
						Template: "Revise the draft using the critique. Reply with the revised text only.\n\nTask: {input}\n\nDraft:\n{steps.draft.output}\n\nCritique:\n{steps.critique.output}"},
					{ID: "save", Kind: KindOutput, Name: "Save", To: ToFile, File: "revised-{date}.md", Template: "{steps.revise.output}"},
				}},
		},
		{
			Key: "plan-delegate-merge", Name: "Plan → Delegate → Merge",
			Description: "Split a job into items, hand each to a model, then merge the results.",
			Flow: Flow{Name: "Plan → Delegate → Merge", Trigger: manual, InputHint: "A job to split up",
				Description: "Plans with one model, delegates the items, merges the results.",
				Steps: []Step{
					{ID: "plan", Kind: KindPlan, Name: "Plan", Model: "default", MaxItems: 5,
						Template: "Split this job into independent parts:\n\n{input}"},
					{ID: "delegate", Kind: KindFanOut, Name: "Delegate", From: "plan", As: KindPrompt, DefaultModel: "default",
						Template: "{item.task}\n\nThis is one part ({item.title}) of a larger job: {input}"},
					{ID: "merge", Kind: KindPrompt, Name: "Merge", Model: "default",
						Template: "Merge these parts into one coherent result for the job: {input}\n\nPlan:\n{steps.plan.output}\n\nParts:\n{steps.delegate.output}"},
					{ID: "notify", Kind: KindOutput, Name: "Notify", To: ToNotify, Template: "Done: {input}"},
				}},
		},
		{
			Key: "website", Name: "Website",
			Description: "Plan a site with a big model, fan out HTML/CSS and copy to local models, assemble in a session, approve, output.",
			Flow: Flow{Name: "Website", Trigger: manual, InputHint: "Describe the website",
				Description: "Plans a small website, delegates its parts, assembles it in an agent session and asks before finishing.",
				Limits:      Limits{MaxParallel: 2},
				Steps: []Step{
					{ID: "plan", Kind: KindPlan, Name: "Plan the site", Model: "default", MaxItems: 6,
						Template: "Plan a small static website: {input}\n\nUse kind \"html\" for page markup, \"css\" for styles and \"copy\" for text. Each task must say exactly what file or text to produce."},
					{ID: "parts", Kind: KindFanOut, Name: "Write the parts", From: "plan", As: KindPrompt, DefaultModel: "default",
						Routes:   []Route{{Kind: "html", Model: "default"}, {Kind: "css", Model: "default"}, {Kind: "copy", Model: "default"}},
						Template: "You are writing one part of a website ({input}).\nPart: {item.title} ({item.kind})\n\n{item.task}\n\nReply with the content only."},
					{ID: "assemble", Kind: KindSession, Name: "Assemble", Agent: "cerberus", Workspace: WorkspaceScratch,
						Task: "Assemble a static website in the current folder from these parts. Write index.html and style.css, fix anything inconsistent, and check that the page opens.\n\nThe site: {input}\n\nParts:\n{steps.parts.output}"},
					{ID: "check", Kind: KindIf, Name: "Did it pass?", Test: TestState, On: "assemble", Value: "passed", Else: End},
					{ID: "approve", Kind: KindApprove, Name: "Approve", Message: "The website for \"{input}\" is assembled. Approve to finish?"},
					{ID: "done", Kind: KindOutput, Name: "Tell me", To: ToInbox,
						Template: "Website ready: {input}\n\nThe files are in the Assemble session's folder; the run page links to it."},
				}},
		},
	}
}
