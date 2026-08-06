package controlplane

// AgentProtocolVersion is the control-plane payload version understood by the
// current agent. Bump it only when an older agent would mishandle a changed
// payload; a bump makes every already-running instance ineligible for new jobs.
const AgentProtocolVersion = 2
