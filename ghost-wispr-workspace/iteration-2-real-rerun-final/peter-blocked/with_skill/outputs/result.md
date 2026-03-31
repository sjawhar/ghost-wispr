# Peter's Notes on Prioritizing Blocked Issues on GitHub Board

## Source
- **Session**: "Nine-hour deadline task triage and QA alignment"
- **Session ID**: 20260327203708
- **Date**: March 27, 2026
- **Speaker**: Peter (peteramcintyre)

---

## Peter's Exact Recommendations

### 1. Core Principle
> "I think the thing that we want is tasks in final review that have no block, like, that have zero block by."

Peter's primary goal is to identify and prioritize tasks that have **zero blocking issues**.

### 2. Definition of "Blocked" (KEY DISTINCTION)

Peter provides a critical clarification on what "blocked" means:

> "So you see blocked by an issue that is currently open rather than blocked by an issue that is closed."

**A task is considered "blocked" ONLY if:**
- It has a "blocked by" field pointing to another issue
- **AND** that blocking issue is still **OPEN** (not closed)

This is the essential framework for prioritization.

### 3. The Proposed Framework

> "And we can do that by saying it is blocked if it has a blocked by and the issue is still open."

**Implementation approach:**
- Use GitHub's "blocked by" field to track dependencies
- Only treat an issue as truly blocked if the blocking issue remains open
- Once a blocking issue is closed, the dependent issue is no longer blocked
- Remove or update the "blocked by" label once the blocking issue is resolved

### 4. Handling Blocked Issues in Workflow

Peter describes the current practice:

> "I've been moving them into the column right column anyway even if they're blocked by things. So that then once they're no longer blocked, we can just say that's approved."

**Strategy:**
- Move tasks to the appropriate column (e.g., "final review") even if they're currently blocked
- Once the blocking issue is resolved, the task can be immediately approved without re-triage
- This reduces friction in the workflow

### 5. Prioritization Methodology

> "What I'm gonna do is I'm going to count up how many issues a block and which tasks, and then I can get back to you on a prioritization, which ones."

Peter's approach to prioritization:
1. Count how many issues are blocking other tasks
2. Identify which specific tasks are affected by each blocker
3. Use this data to determine prioritization order
4. Focus on unblocking high-impact blockers first

### 6. Filtering Guidance for Teams

> "Blocking in any status. That that pull line for the light is blocked by, the issue that is blocking it is not yet closed."

**For Legion or automated systems:**
- Filter for tasks with "blocked by" field populated
- Only include tasks where the blocking issue is still open
- This ensures accurate identification of truly blocked work

### 7. Record Keeping

Peter emphasizes maintaining historical context:

> "I think it's useful to have the record... Yeah. So, like, I think just, like, I don't know, everyone picking these and running at it and yeah, assigning them themselves to them on the Kanban board."

**Best practice:**
- Keep the blocking relationship in issue history even after resolution
- This provides context for future work and prevents duplicate blocking issues
- The history is linked in GitHub's issue tracking

---

## Context: What "Blocked" Means in This Scenario

In this session, Peter is managing a high-pressure deadline (9 hours remaining) with multiple tasks in various stages:
- **Final Review**: Tasks ready for approval but may have blocking dependencies
- **QA**: Tasks undergoing quality assurance
- **Fixes Needed**: Tasks with identified issues to resolve

The "blocked by" relationship indicates that a task cannot proceed until a specific issue is resolved. This could be:
- A bug that needs fixing
- A feature that needs to be completed
- A dependency that needs to be resolved
- An approval or review that needs to happen

---

## Summary: Peter's Prioritization Framework

| Aspect | Peter's Recommendation |
|--------|------------------------|
| **Definition of Blocked** | Has "blocked by" field + blocking issue is OPEN |
| **Primary Goal** | Identify tasks with zero blocking issues |
| **Workflow** | Move tasks to final review even if blocked; unblock later |
| **Prioritization** | Count blockers; prioritize high-impact blockers first |
| **Implementation** | Use GitHub "blocked by" field; update when blocker closes |
| **Record Keeping** | Maintain blocking history for context |
| **Team Communication** | Filter for truly blocked work (open blockers only) |

---

## Key Insight

The critical insight from Peter's discussion is the **distinction between "has a blocked by field" and "is actually blocked"**. A task with a "blocked by" field pointing to a **closed** issue is no longer blocked and can proceed. This distinction is essential for accurate prioritization and workflow management.
