package extractor

// CustomInstructions is passed to mem0.add_memory as the `custom_instructions`
// argument. mem0's internal extractor uses it to steer what gets memorized.
//
// The wording here is deliberately surgical:
//   - "Extract memories about" sets the inclusion list (the four memory types
//     from the user's auto-memory taxonomy: user / feedback / project / reference).
//   - "Avoid memorizing" sets the exclusion list — explicitly naming the
//     noise patterns we already filtered upstream, as a belt-and-suspenders
//     in case our filter misses something.
//   - The "Each memory should be one fact, one sentence" rule keeps mem0
//     from emitting long paragraph-shaped memories that bloat search results.
//   - Third-person framing ("the user prefers...") matches the auto-memory
//     style and reads naturally when /recall surfaces them later.
const CustomInstructions = `
Extract memories about:
- User preferences, working style, role, or expertise
- Project decisions, technical choices, architecture rationale
- References to external resources (URLs, file paths, system names)
- Corrections or feedback the user has given to AI assistants

Avoid memorizing:
- Conversational filler ("ok", "thanks", "let me check")
- Tool execution details (commands run, file reads) unless they revealed
  a non-obvious constraint
- One-off debugging steps that did not change the user's mental model
- Restating things already in code or documentation

Each memory should be one fact, one sentence. Include the project or repo
name when the memory is project-specific. Use third person ("the user
prefers...", "the project requires..."). Never copy code blocks; describe
them.
`
