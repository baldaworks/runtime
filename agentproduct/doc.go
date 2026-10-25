// Package agentproduct defines the provider-neutral runtime product returned by
// capability-aware agent factories.
//
// A Product owns its Agent and any resources reachable through that Agent.
// Callers must close the Product, not Product.Agent. Closing the Agent directly
// bypasses control admission and violates the ownership contract.
package agentproduct
