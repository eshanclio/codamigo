package fixture

// Server handles HTTP connections.
type Server struct {
	host string
	port int
}

// Start begins listening for connections on the configured host and port.
func (s *Server) Start() error {
	if s.host == "" {
		s.host = "localhost"
	}
	return nil
}

// Stop gracefully shuts down the server and resets its state.
func (s *Server) Stop() error {
	if s.host == "" {
		return nil
	}
	s.host = ""
	return nil
}

// NewServer constructs a Server with the given host and port.
func NewServer(host string, port int) *Server {
	return &Server{host: host, port: port}
}
