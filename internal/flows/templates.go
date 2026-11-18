package flows

import "carino.systems/cerberus/internal/evals"

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
// answers fresh copies. Parts written by a fan-out see the whole plan
// (and the Website parts the outline), so they agree with each other.
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
						Template: "Split this job into independent parts that do not overlap. Each part's task must be " +
							"self-contained: say exactly what to produce, and anything it must agree on with the other " +
							"parts (names, terms, formats).\n\nThe job:\n{input}"},
					{ID: "delegate", Kind: KindFanOut, Name: "Delegate", From: "plan", As: KindPrompt, DefaultModel: "default",
						Template: "{item.task}\n\nThis is one part ({item.title}) of a larger job: {input}\n\n" +
							"The whole plan, so your part fits the others (write only your part; do not repeat " +
							"what another part covers):\n{steps.plan.output}"},
					{ID: "merge", Kind: KindPrompt, Name: "Merge", Model: "default",
						Template: "Merge these parts into one coherent result for the job: {input}\n\nRemove repeats and " +
							"make names and terms agree.\n\nPlan:\n{steps.plan.output}\n\nParts (one section each, in plan order):\n{steps.delegate.output}"},
					{ID: "notify", Kind: KindOutput, Name: "Notify", To: ToNotify, Template: "Done: {input}"},
				}},
		},
		websiteTemplate(manual),
	}
}

// Website prompts. Every writer sees the same outline and the whole plan,
// so the parts agree (ids, class names, nav); the copy is written inside
// the sections, not as loose fragments; and the outline rules out fake
// "official" and copyright claims about brands the site does not own.
const (
	websiteOutline = "You are planning a small one-page static website about: {input}\n\n" +
		"Write the outline every writer will follow:\n" +
		"1. Title and a one-line purpose.\n" +
		"2. Audience and tone.\n" +
		"3. Sections in order (3 to 6). For each: an id (lower-case letters and dashes), a heading, " +
		"and what it says, with the real facts to include.\n" +
		"4. Navigation: one link per section, written as #<id>. Link only to sections in this outline: " +
		"there are no other pages.\n" +
		"5. Design: colours (hex), system font stacks, and the CSS class names the markup will use.\n\n" +
		"Rules: write real content, not placeholder text. If the subject is someone else's brand, product, " +
		"game or organisation, this is an unofficial information or fan page: never call it official, write " +
		"no copyright or \"all rights reserved\" line for it, and invent no contact addresses, prices, dates " +
		"or numbers you are not sure of."
	websitePlan = "Split this website outline into parts for writers.\n" +
		"- One part of kind \"section\" per section of the outline, in order. Its title is the section's " +
		"heading; its task names the section id and says what the section must contain.\n" +
		"- One part of kind \"css\" for style.css.\n" +
		"Do not add parts for pages, a separate copy text, a header or a footer: the assembler writes those.\n\n" +
		"The site: {input}\n\nOutline:\n{steps.outline.output}"
	websitePart = "You are writing one part of a one-page website about: {input}\n\n" +
		"Every writer follows this outline:\n{steps.outline.output}\n\n" +
		"All the parts being written (write only yours; do not repeat what another part says):\n{steps.plan.output}\n\n" +
		"Your part: {item.title} ({item.kind})\n{item.task}\n\n" +
		"A section: reply with one <section id=\"...\"> element using the outline's id and class names, with its " +
		"finished text in it. No <html>, <head>, <nav> or <footer>.\n" +
		"The css: reply with the whole style.css for the outline's class names and ids, readable on a phone and " +
		"on a desktop.\n" +
		"Reply with the content only: no Markdown fences, no notes."
	websiteAssemble = "Build the website in the current folder from the outline and the parts below.\n" +
		"- index.html: the doctype, a head with the title, a viewport meta and <link rel=\"stylesheet\" " +
		"href=\"style.css\">, a header with the site title and the outline's navigation, the sections in " +
		"outline order, and a short footer (no copyright line for a brand the site does not own).\n" +
		"- style.css: the css part, fixed where it does not match the markup.\n" +
		"Keep the parts' text, remove repeated text, and make every nav link point at a section id that exists.\n" +
		"Work plainly: write each file once, read it back once to check it, fix only what is wrong, and do " +
		"not write a file again with the same content. End with a short note of what you made.\n\n" +
		"The site: {input}\n\nOutline:\n{steps.outline.output}\n\nParts (one section each, in plan order):\n{steps.parts.output}"
)

// websiteTemplate is the Website template: outline → plan → parts →
// assemble → review (a judge with one round of feedback) → approve.
func websiteTemplate(manual Trigger) Template {
	files := []string{evals.EvidenceFiles}
	return Template{
		Key: "website", Name: "Website",
		Description: "Outline a one-page site with a big model, write its sections and CSS from the same outline, assemble them in a session, review, approve.",
		Flow: Flow{Name: "Website", Trigger: manual, InputHint: "Describe the website",
			Description: "Outlines a small website, writes its parts from one outline, assembles them in an agent session, reviews the result and asks before finishing.",
			Limits:      Limits{MaxParallel: 2},
			Steps: []Step{
				{ID: "outline", Kind: KindPrompt, Name: "Outline the site", Model: "default", Template: websiteOutline},
				{ID: "plan", Kind: KindPlan, Name: "Plan the parts", Model: "default", MaxItems: 7, Template: websitePlan},
				{ID: "parts", Kind: KindFanOut, Name: "Write the parts", From: "plan", As: KindPrompt, DefaultModel: "default",
					Routes:   []Route{{Kind: "section", Model: "default"}, {Kind: "css", Model: "default"}},
					Template: websitePart},
				{ID: "assemble", Kind: KindSession, Name: "Assemble", Agent: "cerberus", Workspace: WorkspaceScratch,
					Task: websiteAssemble},
				{ID: "check", Kind: KindIf, Name: "Did it finish?", Test: TestState, On: "assemble", Value: "passed", Else: End},
				{ID: "review", Kind: KindJudge, Name: "Review the site", On: "assemble",
					Gates: []evals.Gate{{ID: "site", Name: "The site", MaxIterations: 2}},
					Criteria: []evals.Criterion{
						{ID: "files", Gate: "site", Evidence: files,
							Prompt: "index.html and style.css exist, and index.html links style.css."},
						{ID: "nav", Gate: "site", Evidence: files, DependsOn: []string{"files"},
							Prompt: "Every link in index.html's navigation points at a section id that exists in the page; no link points at a page that does not exist."},
						{ID: "content", Gate: "site", Evidence: files, DependsOn: []string{"files"},
							Prompt: "The page has several sections of real text about the subject: no placeholder text, no paragraph said twice, no Markdown fences or stray notes."},
						{ID: "honest", Gate: "site", Evidence: files, DependsOn: []string{"files"},
							Prompt: "The page does not call itself official and has no copyright or \"all rights reserved\" line for a brand, product or game it does not own."},
					}},
				{ID: "approve", Kind: KindApprove, Name: "Approve",
					Message: "The website for \"{input}\" is assembled. Review passed: {steps.review.output.passed}. Approve to finish?"},
				{ID: "done", Kind: KindOutput, Name: "Tell me", To: ToInbox,
					Template: "Website ready: {input}\n\nFolder: {steps.assemble.output.workspace}\n\n{steps.assemble.output.final}"},
			}},
	}
}
