package hapi

import (
	log "code.google.com/p/log4go"
	"coordinator"
	"encoding/json"
	"engine"
	"github.com/bmizerany/pat"
	"io/ioutil"
	"net"
	"net/http"
	"protocol"
	"strings"
)

type HttpServer struct {
	conn        net.Listener
	config      *Configuration
	engine      engine.EngineI
	coordinator coordinator.Coordinator
	shutdown    chan bool
}

func NewHttpServer(config *Configuration, theEngine engine.EngineI, theCoordinator coordinator.Coordinator) *HttpServer {
	self := &HttpServer{}
	self.config = config
	self.engine = theEngine
	self.coordinator = theCoordinator
	self.shutdown = make(chan bool)
	return self
}

func (self *HttpServer) ListenAndServe() {
	conn, err := net.Listen("tcp", self.config.HttpAddr)
	if err != nil {
		log.Error("Listen: ", err)
	}
	self.Serve(conn)
}

func (self *HttpServer) Serve(listener net.Listener) {
	self.conn = listener
	p := pat.New()

	// Run the given query and return an array of series or a chunked response
	// with each batch of points we get back
	p.Get("/api/db/:db/series", http.HandlerFunc(self.query))

	// Write points to the given database
	p.Post("/api/db/:db/series", CorsHeaderHandler(self.writePoints))

	if err := http.Serve(listener, p); err != nil && !strings.Contains(err.Error(), "closed network") {
		panic(err)
	}
	self.shutdown <- true
}

func (self *HttpServer) Close() {
	log.Info("Closing http server")
	self.conn.Close()
	log.Info("Waiting for all requests to finish before killing the process")
	<-self.shutdown
}

type Writer interface {
	yield(*protocol.Series) error
	done()
}

type AllPointsWriter struct {
	memSeries map[string]*protocol.Series
	w         http.ResponseWriter
}

func (self *AllPointsWriter) yield(series *protocol.Series) error {
	oldSeries := self.memSeries[*series.Name]
	if oldSeries == nil {
		self.memSeries[*series.Name] = series
		return nil
	}

	oldSeries.Points = append(oldSeries.Points, series.Points...)
	return nil
}

func (self *AllPointsWriter) done() {
	data, err := serializeMultipleSeries(self.memSeries)
	if err != nil {
		self.w.Write([]byte(err.Error()))
		self.w.WriteHeader(http.StatusInternalServerError)
		return
	}
	self.w.Write(data)
	self.w.WriteHeader(http.StatusOK)
}

type ChunkWriter struct {
	w http.ResponseWriter
}

func (self *ChunkWriter) yield(series *protocol.Series) error {
	data, err := serializeSingleSeries(series)
	if err != nil {
		return err
	}
	self.w.Write(data)
	self.w.WriteHeader(http.StatusOK)
	self.w.(http.Flusher).Flush()
	return nil
}

func (self *ChunkWriter) done() {
}

func (self *HttpServer) query(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	db := r.URL.Query().Get(":db")
	var writer Writer
	if r.URL.Query().Get("chunked") == "true" {
		writer = &ChunkWriter{w}
	} else {
		writer = &AllPointsWriter{map[string]*protocol.Series{}, w}
	}
	err := self.engine.RunQuery(db, query, writer.yield)
	if err != nil {
		w.Write([]byte(err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writer.done()
}

// [
//   {"name": "seriesname", "columns": ["count", "type"], "points": [[3, "asdf"], [1, "foo"]]},
//   {}
// ]

// [
//   {"name": "seriesname", "columns": ["time", "email"], "points": [[], []]}
// ]
func (self *HttpServer) writePoints(w http.ResponseWriter, r *http.Request) {
	db := r.URL.Query().Get(":db")

	series, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	serializedSeries := []*SerializedSeries{}
	err = json.Unmarshal(series, &serializedSeries)
	if err != nil {
		w.Write([]byte(err.Error()))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// convert the wire format to the internal representation of the time series
	for _, s := range serializedSeries {
		if len(s.Points) == 0 {
			continue
		}

		fields := []*protocol.FieldDefinition{}
		for idx, column := range s.Columns {
			var fieldType protocol.FieldDefinition_Type
			switch s.Points[0][idx].(type) {
			case int:
				fieldType = protocol.FieldDefinition_INT64
			case float64:
				fieldType = protocol.FieldDefinition_DOUBLE
			case string:
				fieldType = protocol.FieldDefinition_STRING
			case bool:
				fieldType = protocol.FieldDefinition_BOOL
			}

			_column := column
			fields = append(fields, &protocol.FieldDefinition{
				Name: &_column,
				Type: &fieldType,
			})
		}

		points := []*protocol.Point{}
		for _, point := range s.Points {
			values := []*protocol.FieldValue{}
			var timestamp *int64
			for idx, field := range fields {
				if s.Columns[idx] == "time" {
					_timestamp := point[idx].(int64)
					timestamp = &_timestamp
				}

				switch *field.Type {
				case protocol.FieldDefinition_STRING:
					if str, ok := point[idx].(string); ok {
						values = append(values, &protocol.FieldValue{StringValue: &str})
						continue
					}
				case protocol.FieldDefinition_INT64:
					if integer, ok := point[idx].(int); ok {
						temp := int64(integer)
						values = append(values, &protocol.FieldValue{Int64Value: &temp})
						continue
					}
				case protocol.FieldDefinition_DOUBLE:
					if double, ok := point[idx].(float64); ok {
						values = append(values, &protocol.FieldValue{DoubleValue: &double})
						continue
					}
				case protocol.FieldDefinition_BOOL:
					if boolean, ok := point[idx].(bool); ok {
						values = append(values, &protocol.FieldValue{BoolValue: &boolean})
						continue
					}
				}

				// if we reached this line then the dynamic type didn't match
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			points = append(points, &protocol.Point{
				Values:    values,
				Timestamp: timestamp,
			})
		}

		series := &protocol.Series{
			Name:   &s.Name,
			Fields: fields,
			Points: points,
		}

		self.coordinator.WriteSeriesData(db, series)
	}
}

type Point struct {
	Timestamp      int64         `json:"timestamp"`
	SequenceNumber uint32        `json:"sequenceNumber"`
	Values         []interface{} `json:"values"`
}

type SerializedSeries struct {
	Name    string          `json:"name"`
	Columns []string        `json:"columns"`
	Points  [][]interface{} `json:"points"`
}

func serializeSingleSeries(series *protocol.Series) ([]byte, error) {
	arg := map[string]*protocol.Series{"": series}
	return json.Marshal(serializeSeries(arg)[0])
}

func serializeMultipleSeries(series map[string]*protocol.Series) ([]byte, error) {
	return json.Marshal(serializeSeries(series))
}

func serializeSeries(memSeries map[string]*protocol.Series) []*SerializedSeries {
	serializedSeries := []*SerializedSeries{}

	for _, series := range memSeries {
		columns := []string{"time", "sequence_number"}
		for _, field := range series.Fields {
			columns = append(columns, *field.Name)
		}

		points := [][]interface{}{}
		for _, row := range series.Points {
			rowValues := []interface{}{*row.Timestamp, *row.SequenceNumber}
			for idx, value := range row.Values {
				switch *series.Fields[idx].Type {
				case protocol.FieldDefinition_STRING:
					rowValues = append(rowValues, *value.StringValue)
				case protocol.FieldDefinition_INT32:
					rowValues = append(rowValues, *value.IntValue)
				case protocol.FieldDefinition_INT64:
					rowValues = append(rowValues, *value.Int64Value)
				case protocol.FieldDefinition_DOUBLE:
					rowValues = append(rowValues, *value.DoubleValue)
				case protocol.FieldDefinition_BOOL:
					rowValues = append(rowValues, *value.BoolValue)
				}
			}
			points = append(points, rowValues)
		}

		serializedSeries = append(serializedSeries, &SerializedSeries{
			Name:    *series.Name,
			Columns: columns,
			Points:  points,
		})
	}
	return serializedSeries
}
