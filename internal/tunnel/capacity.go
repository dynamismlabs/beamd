package tunnel

// AgentMaxStreams is the number of concurrent data streams a current agent
// can accept on one transport session. It is advertised during the control
// handshake so newer edges can preserve the lower ceiling for older agents.
const AgentMaxStreams = 128
