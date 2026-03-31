# Peter's Framework for Handling Blocked Issues on GitHub Board

## Source
**Session ID:** 20260327203708  
**Title:** "Nine-hour deadline task triage and QA alignment"  
**Date:** 2026-03-27  
**Context:** Project management discussion about prioritizing tasks with blocking dependencies

---

## Definition of "Blocked" (Peter's Framework)

Peter established a clear, specific definition for what constitutes a "blocked" issue:

> **"It is blocked if it has a blocked by and the issue is still open."**

This is the critical distinction Peter made:
- An issue is **blocked** if it has a "blocked by" relationship AND the blocking issue is **still open**
- An issue is **NOT blocked** if the blocking issue has been **closed**

### Key Quote (Exact):
> "So you see blocked by an issue that is currently open rather than blocked by an issue that is closed."

---

## Prioritization Framework for Blocked Issues

### 1. **Separate Blocked from Unblocked Work**

Peter emphasized the importance of distinguishing between:
- Tasks in final review that have **zero "blocked by"** relationships
- Tasks that are blocked by open issues

> "I think the thing that we want is tasks in final review that have no block, like, that have zero block by."

### 2. **Filter by Blocking Issue Status**

The key insight is that the "blocked by" field alone is insufficient. You must check:
- Whether the blocking issue is **still open** (truly blocked)
- Whether the blocking issue has been **closed** (no longer blocked)

Peter noted a practical problem:
> "The has blocked by thing is annoying because it only includes well, also includes ones that have been unblocked. Like, look. This this one is blocked. This one is not blocked. But just because it has a task that was was blocked by."

### 3. **Recommended Workflow for Legion/Automation**

Peter suggested telling automation systems (like Legion) to:
> "Look at this bit. So you see blocked by an issue that is currently open rather than blocked by an issue that is closed."

The logic should be:
```
IF (issue has "blocked by" field) AND (blocking issue status == "open")
THEN issue is truly blocked
ELSE issue is not blocked
```

### 4. **Handling Closed Blocking Issues**

Peter acknowledged that keeping the "blocked by" record is useful for history:
> "But I think it's useful to have the record. No? Is it? Guess it'd still be linked in the history."

However, the key is that once a blocking issue is closed, the task should no longer be considered blocked for prioritization purposes.

---

## Practical Recommendations

### For Task Assignment
- Prioritize tasks that are **not blocked by open issues**
- Tasks blocked by closed issues can be worked on immediately
- Use the "blocked by" field to track dependencies, but filter by blocking issue status for actual blocking status

### For Board Management
- Don't rely solely on the "blocked by" field to determine if something is blocked
- Always check if the blocking issue is still open
- Once a blocking issue is closed, the dependent task should be moved forward

### For Automation/Legion Integration
Peter's exact guidance:
> "And we can do that by saying it is blocked if it has a blocked by and the issue is still open."

This should be the rule for any system determining which tasks are available to work on.

---

## Context: Why This Matters

This discussion occurred during a 9-hour deadline crunch where:
- Multiple tasks were blocked by open issues
- The team needed to identify which tasks could actually be worked on
- There was confusion about whether "blocked by" field alone indicated true blocking status
- The distinction between "has a blocked by relationship" and "is actually blocked" became critical for prioritization

Peter's framework ensures that:
1. Blocking dependencies are tracked (for history and understanding)
2. Only truly blocked tasks (blocked by open issues) are excluded from work
3. Tasks blocked by closed issues can be immediately unblocked and worked on
