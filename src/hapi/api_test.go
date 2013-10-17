package hapi

import (
	"bytes"
	"common"
	"encoding/json"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"net"
	"net/http"
	"net/url"
	"parser"
	"protocol"
	"testing"
	"time"
)

// Hook up gocheck into the gotest runner.
func Test(t *testing.T) {
	TestingT(t)
}

type ApiSuite struct {
	listener    net.Listener
	server      *HttpServer
	coordinator *MockCoordinator
}

var _ = Suite(&ApiSuite{})

type MockEngine struct{}

func (self *MockEngine) RunQuery(_ string, query string, yield func(*protocol.Series) error) error {
	series, err := common.StringToSeriesArray(`
[
  {
    "points": [
      {
        "values": [
				  { "string_value": "some_value"},{"int_value": 1}
        ],
        "timestamp": 1381346631,
        "sequence_number": 1
      },
      {
        "values": [
				  {"string_value": "some_value"},{"int_value": 2}
				],
        "timestamp": 1381346632,
        "sequence_number": 2
      }
    ],
    "name": "foo",
    "fields": [{"type": "STRING","name": "column_one"},{"type": "INT32","name": "column_two"}]
  },
  {
    "points": [
      {
        "values": [
				  { "string_value": "some_value"},{"int_value": 3}
        ],
        "timestamp": 1381346633,
        "sequence_number": 1
      },
      {
        "values": [
				  {"string_value": "some_value"},{"int_value": 4}
				],
        "timestamp": 1381346634,
        "sequence_number": 2
      }
    ],
    "name": "foo",
    "fields": [{"type": "STRING","name": "column_one"},{"type": "INT32","name": "column_two"}]
  }
]
`)
	if err != nil {
		return err
	}
	if err := yield(series[0]); err != nil {
		return err
	}
	return yield(series[1])
}

type MockCoordinator struct {
	series []*protocol.Series
}

func (self *MockCoordinator) DistributeQuery(db string, query *parser.Query, yield func(*protocol.Series) error) error {
	return nil
}
func (self *MockCoordinator) WriteSeriesData(db string, series *protocol.Series) error {
	self.series = append(self.series, series)
	return nil
}

func (self *ApiSuite) SetUpSuite(c *C) {
	self.coordinator = &MockCoordinator{}
	self.server = NewHttpServer(nil, &MockEngine{}, self.coordinator)
	var err error
	self.listener, err = net.Listen("tcp4", ":")
	c.Assert(err, IsNil)
	go func() {
		self.server.Serve(self.listener)
	}()
	time.Sleep(1 * time.Second)
}

func (self *ApiSuite) TearDownSuite(c *C) {
	self.server.Close()
}

func (self *ApiSuite) TestNotChunkedQuery(c *C) {
	port := self.listener.Addr().(*net.TCPAddr).Port
	query := "select * from foo where column_one == 'some_value';"
	query = url.QueryEscape(query)
	addr := fmt.Sprintf("http://localhost:%d/api/db/foo/series?q=%s", port, query)
	resp, err := http.Get(addr)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	series := []SerializedSeries{}
	err = json.Unmarshal(data, &series)
	c.Assert(err, IsNil)
	c.Assert(series, HasLen, 1)
	c.Assert(series[0].Name, Equals, "foo")
	// time, seq, column_one, column_two
	c.Assert(series[0].Columns, HasLen, 4)
	c.Assert(series[0].Points, HasLen, 4)
}

func (self *ApiSuite) TestChunkedQuery(c *C) {
	port := self.listener.Addr().(*net.TCPAddr).Port
	query := "select * from foo where column_one == 'some_value';"
	query = url.QueryEscape(query)
	addr := fmt.Sprintf("http://localhost:%d/api/db/foo/series?q=%s&chunked=true", port, query)
	resp, err := http.Get(addr)
	c.Assert(err, IsNil)
	defer resp.Body.Close()

	for i := 0; i < 2; i++ {
		chunk := make([]byte, 2048, 2048)
		n, err := resp.Body.Read(chunk)
		c.Assert(err, IsNil)

		series := SerializedSeries{}
		err = json.Unmarshal(chunk[0:n], &series)
		c.Assert(err, IsNil)
		c.Assert(series.Name, Equals, "foo")
		// time, seq, column_one, column_two
		c.Assert(series.Columns, HasLen, 4)
		// each chunk should have 2 points
		c.Assert(series.Points, HasLen, 2)
	}
}

func (self *ApiSuite) TestWriteData(c *C) {
	data := `
[
  {
    "points": [
				["1", 1, 1.0, true],
				["2", 2, 2.0, false],
				["3", 3, 3.0, true]
    ],
    "name": "foo",
    "columns": ["column_one", "column_two", "column_three", "column_four"]
  }
]
`

	port := self.listener.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("http://localhost:%d/api/db/foo/series", port)
	resp, err := http.Post(addr, "application/json", bytes.NewBufferString(data))
	c.Assert(err, IsNil)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	fmt.Printf("body: %s\n", string(body))
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	c.Assert(self.coordinator.series, HasLen, 1)
	series := self.coordinator.series[0]
	c.Assert(series.Fields, HasLen, 4)

	// check the types
	c.Assert(*series.Fields[0].Name, Equals, "column_one")
	c.Assert(*series.Fields[0].Type, Equals, protocol.FieldDefinition_STRING)
	// TODO: cannot get an int64 from a json object
	c.Assert(*series.Fields[1].Name, Equals, "column_two")
	c.Assert(*series.Fields[1].Type, Equals, protocol.FieldDefinition_DOUBLE)
	c.Assert(*series.Fields[2].Name, Equals, "column_three")
	c.Assert(*series.Fields[2].Type, Equals, protocol.FieldDefinition_DOUBLE)
	c.Assert(*series.Fields[3].Name, Equals, "column_four")
	c.Assert(*series.Fields[3].Type, Equals, protocol.FieldDefinition_BOOL)

	// check the values
	c.Assert(series.Points, HasLen, 3)
	c.Assert(*series.Points[0].Values[0].StringValue, Equals, "1")
	c.Assert(*series.Points[0].Values[1].DoubleValue, Equals, 1.0)
	c.Assert(*series.Points[0].Values[2].DoubleValue, Equals, 1.0)
	c.Assert(*series.Points[0].Values[3].BoolValue, Equals, true)
}
