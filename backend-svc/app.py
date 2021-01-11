from flask import request
from flask_api import FlaskAPI, status, exceptions
from psycopg2 import pool
import os
import atexit
from neo4j import GraphDatabase
import neo4j

app = FlaskAPI(__name__)

def setup():
  global stop_info_query
  stop_info_query = """SELECT stop_name, stop_lat, stop_lon, stop_id
                       FROM stop
                       WHERE stop_id=%(id)s"""
  global closest_stop_query
  closest_stop_query = """SELECT stop_id, stop_name, stop_lat, stop_lon
                          FROM stop
                          ORDER BY stop.geom <-> ST_GeometryFromText('POINT(%(lon)s %(lat)s)', 4326)
                          LIMIT 5
                       """
  global routes_for_stop_query 
  routes_for_stop_query = """SELECT DISTINCT r.route_short_name, r.route_long_name
                             FROM route as r
                             INNER JOIN trip as t on t.route_id=r.route_id
                             INNER JOIN stop_time as st on st.trip_id=t.trip_id
                             WHERE st.stop_id=%(stop_id)s
                          """
  global routes_for_multiple_stops_query
  routes_for_multiple_stops_query = """SELECT DISTINCT r.route_short_name, r.route_long_name, st.stop_id
                             FROM route as r
                             INNER JOIN trip as t on t.route_id=r.route_id
                             INNER JOIN stop_time as st on st.trip_id=t.trip_id
                             WHERE st.stop_id IN %(stop_ids)s
                             ORDER BY st.stop_id
                             """
  global path_between_stops_query
  path_between_stops_query = """MATCH (start:Stop{id: $start_id}) MATCH (end:Stop{id: $end_id}) call apoc.path.expandConfig(start, {relationshipFilter: "NEXT>|LOCATED", endNodes: [end], uniqueness: "NODE_GLOBAL"}) yield path with path limit 1 return nodes(path);
"""
  # DB setup
  global conn_pool
  pg_db_user = os.environ.get('PG_USER')
  pg_pw = os.environ.get('PG_PW')
  pg_host = os.environ.get('PG_HOST')
  pg_port = os.environ.get('PG_PORT')
  pg_db = os.environ.get('PG_DB')
  conn_pool = pool.ThreadedConnectionPool(1, 15, user=pg_db_user, password=pg_pw, host=pg_host, port=pg_port, database=pg_db)
  if not conn_pool:
    exit(2)
  neo4j_uri = os.environ.get('NEO_URI')
  neo4j_pw = os.environ.get('NEO_PW')
  neo4j_user = os.environ.get('NEO_USER')
  global neo4j_driver
  neo4j_driver = GraphDatabase.driver(neo4j_uri, auth=(neo4j_user, neo4j_pw))
  atexit.register(cleanup)

@app.route('/path', methods=['GET'])
def path():
  resp = {'trips': [], 'error': ''}
  if 'start' not in request.args:
    resp['error'] = 'Missing starting stop ID'
    return resp, status.HTTP_400_BAD_REQUEST
  if 'end' not in request.args:
    resp['error'] = 'Missing ending stop ID'
    return resp, status.HTTP_400_BAD_REQUEST
  starting_stop_id = convert_to_int(request.args['start'])
  if starting_stop_id is None:
    resp['error'] = 'Invalid starting stop ID, got '+request.args['start']
    return resp, status.HTTP_400_BAD_REQUEST
  ending_stop_id = convert_to_int(request.args['end'])
  if ending_stop_id is None:
    resp['error'] = 'Invalid ending stop ID, got '+request.args['end']
    return resp, status.HTTP_400_BAD_REQUEST
  full_path = []
  transfers = []
  with neo4j_driver.session(default_access_mode=neo4j.READ_ACCESS) as session:
    result = session.run(path_between_stops_query, start_id=starting_stop_id, end_id=ending_stop_id)
    for record in result:
      trip = {'legs': [], 'transfers': []}
      for idx, nodes in enumerate(record.values()):
        leg = []
        for inner_idx, node in enumerate(nodes):
          if 'StopForRoute' in node.labels:
            leg.append({'stopId': node.get('stopId'), 'stopName': node.get('stopName'), 'routeShortName': node.get('routeShortName'), 'sequenceNum': node.get('sequenceNum'), 'lon': node.get('stopLon'), 'lat': node.get('stopLat')})
          else:
            if len(leg) > 0:
                full_path.append(leg)
                leg = []
                if inner_idx != len(nodes)-1:
                    transfers.append({'stopId': node.get('id')})
  trip['legs'] = full_path
  trip['transfers'] = transfers
  resp['trips'].append(trip)
  return resp, status.HTTP_200_OK

@app.route('/stop', methods=['GET'])
def stop():
  resp = {'name': '', 'lat': '', 'lon': '', 'id': '', 'error': ''}
  if 'id' not in request.args:
    resp['error'] = 'ID not provided'
    return resp, status.HTTP_400_BAD_REQUEST
  id = convert_to_int(request.args['id'])
  if id is None:
    resp['error'] = 'Invalid ID, got '+request.args['id']
    return resp, status.HTTP_400_BAD_REQUEST
  pg_conn = conn_pool.getconn()
  if pg_conn is None:
    resp['error'] = 'Unable to get DB connection'
    return resp, status.HTTP_500_INTERNAL_SERVER_ERROR
  with pg_conn.cursor() as c:
    c.execute(stop_info_query, {'id': id})
    stop = c.fetchall()
  for row in stop:
    resp['name'] = row[0]
    resp['lat'] = row[1]
    resp['lon'] = row[2]
    resp['id'] = row[3]
  return resp, status.HTTP_200_OK


@app.route('/routesforstop', methods=['GET'])
def routes_for_stop():
  resp = {'routes': [], 'error': ''}
  if 'id' not in request.args:
    resp['error'] = 'Stop ID not provided'
    return resp, status.HTTP_400_BAD_REQUEST
  stop_id = convert_to_int(request.args['id'])
  if stop_id is None:
    resp['error'] = 'Invalid stop ID, got '+str(stop_id)
    return resp, status.HTTP_400_BAD_REQUEST
  pg_conn = conn_pool.getconn()
  if pg_conn is None:
    return resp, status.HTTP_500_INTERNAL_SERVER_ERROR
  with pg_conn.cursor() as c:
    c.execute(routes_for_stop_query, {'stop_id': stop_id})
    routes = c.fetchall()
  conn_pool.putconn(pg_conn)
  for row in routes:
    route_short_name = row[0]
    route_long_name = row[1]
    resp['routes'].append({'shortName': route_short_name.strip(), 'longName': route_long_name.strip()})
  return resp, status.HTTP_200_OK

@app.route('/closeststops', methods=['GET'])
def closest_stops_route():
  resp = {'stops': [], 'error': ''}
  if 'lat' not in request.args:
    resp['error'] = 'User latitude not provided'
    return resp, status.HTTP_400_BAD_REQUEST
  if 'lon' not in request.args:
    resp['error'] = 'User longitude not provided'
    return resp, status.HTTP_400_BAD_REQUEST
  user_lat = convert_to_float(request.args['lat'])
  if user_lat is None:
    resp['error'] = 'Bad latitude value; got: '+str(request.args['lat'])
    return resp, status.HTTP_400_BAD_REQUEST
  user_lon = convert_to_float(request.args['lon'])
  if user_lon is None:
    resp['error'] = 'Bad longitude value; got: '+str(request.args['lon'])
    return resp, status.HTTP_400_BAD_REQUEST
  pg_conn = conn_pool.getconn()
  if pg_conn is None:
    return resp, status.HTTP_500_INTERNAL_SERVER_ERROR
  with pg_conn.cursor() as c:
    c.execute(closest_stop_query, {'lon': user_lon, 'lat': user_lat})
    closest_stops = c.fetchall()
    if closest_stops is None:
      return resp, status.HTTP_500_INTERNAL_SERVER_ERROR
  stops = []
  for row in closest_stops:
    stop_id = row[0]
    stop_name = row[1]
    stop_lat = row[2]
    stop_lon = row[3]
    resp['stops'].append({'id': stop_id, 'name': stop_name.strip(), 'lat': stop_lat, 'lon': stop_lon})
    stops.append(stop_id)
  # now get the routes that serve each stop
  with pg_conn.cursor() as c:
    c.execute(routes_for_multiple_stops_query, {'stop_ids': tuple(stops)})
    routes_for_stops = c.fetchall()
    if routes_for_stops is None:
      return resp, status.HTTP_500_INTERNAL_SERVER_ERROR
    stop_to_routes = dict()
    for row in routes_for_stops:
      stop_id = row[2]
      short_name = row[0]
      long_name = row[1]
      if stop_id not in stop_to_routes:
        stop_to_routes.update({stop_id: []})
      stop_to_routes[stop_id].append({'shortName': short_name.strip(), 'longName': long_name.strip()})
  conn_pool.putconn(pg_conn)
  for idx, val in enumerate(stops):
    if val in stop_to_routes:
      print(stop_to_routes[val])
      resp['stops'][idx]['routes'] = stop_to_routes[val]
  return resp, status.HTTP_200_OK


def cleanup():
  if conn_pool is not None:
    conn_pool.closeall()
  neo4j_driver.close()

def convert_to_float(num):
  try:
    float(num)
    return float(num)
  except ValueError:
    return None

def convert_to_int(num):
  try:
    n = int(num)
    return n
  except ValueError:
    print('error converting to int')
    return None

   
if __name__ == "__main__":
    setup()
    app.run(debug=True)


