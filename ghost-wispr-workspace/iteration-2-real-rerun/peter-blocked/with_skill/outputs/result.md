# Peter's Notes on Prioritizing Blocked Issues on GitHub Board

## Source Information
- **Session ID**: 20260328044039
- **Session Title**: "Troubleshooting 1Password UI Issues and Agent Tasks"
- **Date**: March 28, 2026
- **Speaker**: Peter (peteramcintyre)

## Key Discussion: Handling Blocked Issues

### Context
The discussion occurred in the context of managing GitHub issues and tasks, specifically around the "workable" project. Peter was reviewing tasks that were marked as "blocked by" other issues.

### Peter's Exact Notes and Recommendations

**The Problem Identified:**
Peter discovered that he had erroneously added "blocked by" labels to multiple tasks. He explained:

> "I think what happened was it had the workable tag for some reason and then I just went through and added the workable locked by for all of the ones with that tag. But I'm just removing it now. So unblocked."

**The Specific Case:**
- **Task**: "Emails with settlement fraud v1"
- **Issue**: It was labeled as a workable task and marked as "blocked by" workable
- **Peter's Analysis**: "I just I asked I I asked Claude to look through the ones that were flagged and to see which of them this one doesn't use or it won't."
- **Resolution**: Peter determined the blocking label was added erroneously and removed it, unblocking the task

### What "Blocked" Means in This Context
In this context, "blocked" refers to issues/tasks that have a dependency relationship where:
- A task cannot proceed because another task (the blocking issue) must be completed first
- The blocking issue is still open/incomplete
- The blocked task is waiting for the blocking task to be resolved

### Peter's Prioritization Framework
While Peter didn't explicitly state a formal framework, his approach demonstrates:

1. **Verification Before Blocking**: Don't mark tasks as blocked without verifying the actual dependency exists
2. **Regular Auditing**: Review blocked items to ensure the blocking relationship is still valid
3. **Unblock When Possible**: Remove blocking labels when the dependency is no longer valid or was added in error
4. **Contextual Understanding**: Understand the actual relationship between tasks (e.g., "Emails with settlement fraud" doesn't actually depend on workable tasks)

### Key Principle
Peter's statement "So unblocked" and his action of removing the erroneous blocking label demonstrates a principle of **keeping the board clean and accurate** - only marking tasks as blocked when there's a genuine, verified dependency.

## Related Discussion
In the same session, there's a broader principle mentioned: **"Always be unblocking"** - suggesting that a key goal in task management is to actively work on removing blockers and unblocking work.

## Search Methodology
- **Keyword searches used**: "blocked" + "Peter" speaker filter
- **Sessions found**: 2 sessions with Peter discussing blocked issues
- **Primary source**: Session 20260328044039 (refined transcript)
- **Verification**: Confirmed Peter's exact words and context from transcript
