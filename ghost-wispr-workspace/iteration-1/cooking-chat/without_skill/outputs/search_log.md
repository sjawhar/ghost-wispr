# Search Log for Food Science Conversation

## API Calls Made

### Call 1: Get API Specification
```bash
curl -s "http://localhost:8080/api/openapi.json" | jq
```
**Result**: Successfully retrieved OpenAPI 3.1.0 specification with available endpoints including `/api/search` for full-text search.

### Call 2: Search for "browning temperature"
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/search?q=browning%20temperature"
```
**Result**: No results found (empty array).

### Call 3: Search for "food science"
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/search?q=food%20science"
```
**Result**: Found 1 session:
- Session ID: `20260331002536`
- Title: "Casual chat on AI agents, workflows, and food"
- Snippet: "But, yeah, I think that's what I think that's what food science should be. You know? We have these because we can..."

### Call 4: Get full session details for 20260331002536
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/sessions/20260331002536"
```
**Result**: Retrieved 66 segments of conversation transcript (truncated in output).

### Call 5: Search for "Maillard" (browning reaction)
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/search?q=Maillard"
```
**Result**: Found 1 session:
- Session ID: `20260325000501-merged`
- Title: "Office Kitchen Chat on Political Calls and Cooking"
- Snippet: "A casual office kitchen conversation discussing unsolicited political campaign calls, cooking techniques (specifically the Maillard reaction), and meal preparation involving garlic, lemon, and..."

### Call 6: Get context for Maillard in session 20260325000501-merged (uppercase)
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/sessions/20260325000501-merged/context?q=Maillard"
```
**Result**: Error - no match found for query "Maillard"

### Call 7: Get context for maillard in session 20260325000501-merged (lowercase)
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/sessions/20260325000501-merged/context?q=maillard"
```
**Result**: Error - no match found for query "maillard"

### Call 8: Get full session details for 20260325000501-merged
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/sessions/20260325000501-merged"
```
**Result**: Retrieved session with 66 segments.

### Call 9: Check segment count
```bash
curl -s -u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN" "http://localhost:8080/api/sessions/20260325000501-merged" | jq '.segments | length'
```
**Result**: 66 segments

## Summary
- Total API calls made: 9 (within 10 call limit)
- Found the relevant session: `20260325000501-merged`
- Session title: "Office Kitchen Chat on Political Calls and Cooking"
- The session contains discussion about the Maillard reaction, which is the chemical process responsible for browning food
