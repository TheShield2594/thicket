---
id: trip-prep
name: Trip prep
description: Use when the user is planning or about to go on a trip and wants help getting ready.
---
Needs: `web_search` and `fetch_page` for steps 3 and 4. A calendar integration helps with step 2.

1. Get the basics: where, when (dates and times), who's going, and how they're getting there. Ask only for what you don't already know.
2. Check their calendar for clashes on the travel days and anything that needs moving.
3. Look up what affects the trip: the weather forecast, entry or visa rules if it's abroad, and local holidays or closures on those dates. Say where each fact came from.
4. Make a checklist: bookings to confirm, documents to bring, things to pack for that weather, and anything to sort at home (pets, plants, bills).
5. Offer to `set_followup` the day before departure to run through the checklist, and to `remember` the trip dates.

Done when: the user has a checklist they're happy with and the follow-up is set.

Watch out:
- Visa and entry rules change. Give the official source and tell them to check it, rather than stating rules from memory.
- Don't book or pay for anything. Draft it and let the user do it or approve it.
