# marta-web-app
The front-end service will handle all interactions with the UI (index.html). It currently uses the back-end service to get static data and caches the real-time bus info from MARTA's bus API in the background.
The front-end service currently exposes two endpoints:
- `/closeststops`
  - This returns the 5 closest stops to a given `lat` and `lon`, which are passed as query string parameters
- `/closestbuses`
  - This returns all buses that are within a .5 mi radius that meet the following conditions:
    - They serve the closest stop to a given `lat` and `lon` (passed as query string parameters)
    - They are currently on the road, according to MARTA's real-time bus API

The back-end service communicates with the data layer (see: https://github.com/chrisdobbins/neo4j-marta) to retrieve static schedule data.
It currently exposes two endpoints:
- `/closeststops`
  - This returns the 5 closest stops to a given `lat` and `lon`, which are passed as query string parameters
- `/routesforstop`
  - This returns all of the routes that serve a given stop. The stop's `id` is a query string parameter

## TODOs
- Add tests
- Clean up comments and code
- Add an endpoint that returns an ordered list of all stops served by a route's trip
- Integrate the front-end service with the UI
- Deploy it somewhere TBD
- Make the number of stops returned from `/closeststops` configurable
- Make `/closestbuses`'s search radius configurable
