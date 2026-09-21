// Package aurorafleet is the self-host runtime fleet controller.
//
// The main Multica server ships a cloud-runtime HTTP proxy
// (internal/cloudruntime) whose baseURL points at the private multica-cloud
// service. For Aurora's self-host deployment that service is replaced by this
// controller: it exposes the same node path surface
// (GET/POST/DELETE /api/v1/nodes and /nodes/{start,stop,reboot,status,exec})
// and provisions sandbox nodes on demand.
//
// A sandbox node is a container running the Multica daemon. The controller
// injects the server URL and the managed-registration secret into each node so
// its daemon can register as managed (see handler.ManagedRuntimeRegister) and
// claim the workspace's queued Aurora tasks. The Backend interface is the seam
// between that transport/API layer and a concrete node runtime: Docker in
// production, an in-memory registry in tests.
package aurorafleet
