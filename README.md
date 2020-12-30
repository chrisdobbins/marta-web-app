# marta-web-app-frontend
backend repo: https://github.com/chrisdobbins/neo4j-marta/tree/web-app
This is the front-end service for the UI (coming soon). It currently exposes two endpoints:
- `/closeststops`
 - This returns the 5 closest stops to a given `lat` and `lon`, which are passed as query string parameters

- `/closestbuses`
 - This returns all buses within a .5 mi radius that meet the following conditions:
   - They serve the closest stop to a given `lat` and `lon` (passed as query string parameters)
   - They are currently on the road

## TODOs
- Add tests
- Clean up comments

