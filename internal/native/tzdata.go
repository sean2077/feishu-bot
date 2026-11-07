package native

// Release builds use -trimpath, so Windows hosts have no GOROOT zoneinfo.
// Embed the database so a configured TZ such as America/New_York loads.
import _ "time/tzdata"
