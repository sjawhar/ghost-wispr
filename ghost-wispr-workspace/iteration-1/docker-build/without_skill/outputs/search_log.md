# Search Log

## API Calls Made

### Call 1: Get API Specification
```bash
curl -s "http://localhost:8080/api/openapi.json" | jq
```
**Purpose**: Understand available API endpoints and search capabilities
**Result**: Found `/api/search` endpoint for full-text search

### Call 2: Search for Build System Space Problems
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/search?q=build+system+space+out+of+space"
```
**Purpose**: Search for meetings discussing build system space issues
**Result**: Found 1 matching session:
- Session ID: `20260227234044`
- Title: "Office Standup & Docker Issues"
- Snippet: "Experiencing Docker build failures due to disk space problems - Builder running out of space and becoming unresponsive"

### Call 3: Get Full Session Details
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/sessions/20260227234044"
```
**Purpose**: Retrieve complete transcript for the session
**Result**: Retrieved full session with 200+ transcript segments

### Call 4: Get Context Around Space Problem
```bash
curl -s -u "ghost-wispr:${GHOST_WISPR_AUTH_TOKEN}" "http://localhost:8080/api/sessions/20260227234044/context?q=out+of+space"
```
**Purpose**: Get surrounding context for the specific mention of space problems
**Result**: Retrieved context window with 40+ segments around the match at 18.96 seconds

## Summary
- **Total API calls made**: 4
- **Calls remaining**: 6 (out of 10 limit)
- **Session found**: 20260227234044
- **Date**: February 27, 2026 (23:40:44 UTC)
