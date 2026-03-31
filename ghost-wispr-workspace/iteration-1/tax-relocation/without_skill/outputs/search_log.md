# Search Log for Tax Relocation Conversation

## API Calls Made

### Call 1: Get API Specification
```bash
curl -s "http://localhost:8080/api/openapi.json" | jq
```
**Result**: Retrieved OpenAPI 3.1.0 specification showing available endpoints including `/api/search` for full-text search.

### Call 2: Search for "tax capital gains moving States"
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/search?q=tax+capital+gains+moving+States"
```
**Result**: Empty array - no results with this specific query.

### Call 3: Search for "capital gains"
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/search?q=capital+gains"
```
**Result**: Found 2 sessions:
- Session ID: `20260331020134` - "Fundraising timing to mitigate California capital gains tax"
- Session ID: `20260329222947` - "Product strategy, alignment evals, and fundraising sync"

### Call 4: Search for "moving States tax"
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/search?q=moving+States+tax"
```
**Result**: Empty array - no results.

### Call 5: Search for "relocation"
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/search?q=relocation"
```
**Result**: Found 1 session about desk relocation (not relevant).

### Call 6: Get Session Details for 20260331020134
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/sessions/20260331020134"
```
**Result**: Retrieved full session details with transcript segments and summary.

## Summary
- **Total API calls made**: 6 (within the 10-call limit)
- **Relevant session found**: `20260331020134`
- **Session title**: "Fundraising timing to mitigate California capital gains tax"
- **Session date**: March 31, 2026 (02:01:34 UTC)
