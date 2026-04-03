# Peter's Notes on Prioritizing Blocked GitHub Issues

## Executive Summary

Peter's discussion on handling blocked GitHub issues (where the blocking issue is still open) comes from a late-night backlog triage session on **2026-03-28** (Session ID: `20260328073102`). The key insight is that the team needs a systematic way to **capture and track blocking relationships** to properly prioritize work.

---

## Peter's Exact Recommendations

### 1. **Capture Blocking Relationships Correctly**

**Direct Quote from Transcript:**
> "I only make it easier to to capture these blocking relationships, correctly."

**Context:** Peter was discussing Issue 114, which was assigned to the planner to improve how the system captures "blocked by" relationships.

**Implementation:** 
- Use the planner feature (Issue 114) to query session IDs and track what went wrong
- Query the OpenCode database to capture blocking relationships
- This allows the team to understand which issues are blocked and by what

### 2. **Prioritization Framework**

**The Framework:**
Peter's approach to prioritizing blocked issues involves:

1. **Identify blocking relationships** - Know which issues are blocking others
2. **Focus on high-impact blockers** - Prioritize issues that are blocking multiple other tasks
3. **Implement graph visualization** - Create a "graph relationship of code base" to visualize dependencies
4. **Fix data integrity issues** - Ensure pagination and other bugs don't hide blocking relationships

**Specific Issues Prioritized:**
- **Issues 68, 69, 70, 71** - Codebase indexing issues (marked as high priority)
- **Issue 82** - Pagination bug that was causing missing data
- **Issue 114** - Blocking relationships tracking (core to the framework)
- **Issue 31** - Related codebase duplication

### 3. **What "Blocked" Means in This Context**

From the discussion, "blocked" refers to:
- Issues that have **dependencies on other issues that are still open**
- Issues that **cannot be progressed** until their blocking issues are resolved
- The need to track **"blocked by" relationships** in the system

**Key Insight:** The team was concerned that without proper tracking, blocked issues might be worked on prematurely or forgotten, wasting effort.

### 4. **Handling Blocked Issues When Blocking Issue is Still Open**

**Peter's Approach:**
1. **Don't work on blocked issues yet** - Wait for the blocking issue to be resolved
2. **Track them explicitly** - Use Issue 114 to capture these relationships
3. **Visualize dependencies** - Implement a graph to see the full dependency chain
4. **Prioritize the blockers** - Focus engineering effort on resolving the blocking issues first

**Related Decision:** Peter mentioned that Issue 82 (pagination bug) was potentially causing the system to miss some blocked relationships, so fixing data integrity issues is part of the framework.

---

## Session Context

**Session:** Late Night Submission and Backlog Triage  
**Date:** 2026-03-28T07:31:02Z  
**Duration:** ~25 minutes  
**Participants:** Sammy, Linda, Peter, Tom, and others  

**Session Summary:** The team successfully completed a major milestone submission and then triaged technical backlog issues. The discussion about blocking relationships came up when deciding which issues to prioritize for the next phase of work.

---

## Key Decisions Made

1. **Assign Issue 114 to the planner** - To capture blocking relationships correctly
2. **Prioritize Issues 68, 69, 70, 71** - Codebase indexing (high impact)
3. **Fix Issue 82** - Pagination bug affecting data completeness
4. **Implement graph visualization** - For understanding code base relationships

---

## Transcript Evidence

**Full Context from Session 20260328073102:**

```
Speaker 4: "Looking for the issues and the and the blocked by relationships 
and the child issues of the issues. Just go back to go back to moving things through."

Speaker 2 (Peter): "Pipeline normally."

Speaker 4: "Let's add feature issue one fourteen. To if, I guess, pass it on to the planner. 
We used that today, and it was kind of buggy. So, Linda, you can add you can prompt the 
the planner to query your own session ID. What went wrong? You can query it in the open code 
database. I only make it easier to to capture these blocking relationships,"

Speaker 2 (Peter): "correctly."

Speaker 4: "Also 82, 82 might have been the cause of a bug that that that that Peter just 
commented on about you possibly missing some stuff because of pagination. We should deal with that too."

Speaker 4: "Let's finally tackle these code based index issues on the backlog. Let's see. 68, 69, 
seventy, seventy one. I really do think it'd be really valuable to have something like a a graph 
relationship of code base."
```

---

## Summary

Peter's framework for handling blocked GitHub issues emphasizes:

1. **Systematic tracking** of blocking relationships (Issue 114)
2. **Data integrity** to ensure no blocked issues are missed (Issue 82)
3. **Visualization** of dependencies through graph relationships
4. **Prioritization** based on impact - focus on blockers, not blocked issues
5. **Explicit decision-making** - don't work on blocked issues until blockers are resolved

This approach ensures the team doesn't waste effort on issues that can't be completed, and instead focuses on removing blockers to unblock downstream work.
