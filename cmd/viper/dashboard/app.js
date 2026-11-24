(() => {
  "use strict";
  const nodes = document.getElementById("nodes");
  const message = document.getElementById("message");
  const count = document.getElementById("node-count");
  const refreshButton = document.getElementById("refresh");
  const lastRefresh = document.getElementById("last-refresh");
  const previous = new Map();
  let busy = false;
  let timer;

  function element(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined) item.textContent = text;
    return item;
  }
  function label(value, prefix) {
    return String(value || "Unknown").replace(prefix, "").replaceAll("_", " ").toLowerCase().replace(/^./, c => c.toUpperCase());
  }
  function time(value) {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "Time unavailable" : date.toLocaleString();
  }
  function problem(code) {
    return ({ timeout: "Timed out", unauthorized: "Not authorized", unavailable: "Unavailable", error: "Request failed" })[code] || "Request failed";
  }
  function badge(text, kind) { return element("span", `badge ${kind || ""}`, text); }
  function healthKind(health) {
    if (health === "HEALTH_STATE_SERVING") return "good";
    if (health === "HEALTH_STATE_DEGRADED") return "warn";
    if (health === "HEALTH_STATE_NOT_SERVING") return "bad";
    return "";
  }
  function detail(name, value) {
    const item = element("dl", "detail");
    item.append(element("dt", "", name), element("dd", "", value ?? "—"));
    return item;
  }
  function card(observation, fetchFailed) {
    const live = observation.liveness || {};
    let snapshot = live.status ? { status: live.status, at: observation.observed_at } : previous.get(observation.id);
    if (live.status && !fetchFailed) previous.set(observation.id, snapshot);
    const stale = Boolean(snapshot && (!live.status || fetchFailed));
    const status = snapshot && snapshot.status;
    const node = element("article", "node");
    const top = element("div", "node-top");
    const identity = element("div");
    identity.append(element("div", "node-id", observation.id), element("div", "node-caption", "Configured node"));
    top.append(identity, badge(status ? label(status.role, "NODE_ROLE_") : "Unknown", stale ? "warn" : ""));
    node.append(top);
    const health = element("div", "node-status");
    health.append(element("span", "", "Liveness"), badge(status ? label(status.health, "HEALTH_STATE_") : problem(live.error), status ? healthKind(status.health) : "bad"));
    node.append(health);
    const metrics = element("div", "details");
    metrics.append(detail("Term", status?.term), detail("Committed", status?.commit_index), detail("Applied", status?.applied_index));
    node.append(metrics);
    const readiness = observation.readiness || {};
    const ready = readiness.status;
    const readyLine = element("div", "node-status");
    readyLine.append(element("span", "", "Readiness"), badge(ready ? label(ready.health, "HEALTH_STATE_") : problem(readiness.error), ready ? healthKind(ready.health) : "bad"));
    node.append(readyLine);
    if (status?.reason) node.append(element("p", "node-note", status.reason));
    if (stale) node.append(element("p", "node-note stale", `Stale liveness · last observed ${time(snapshot.at)}`));
    else if (snapshot) node.append(element("p", "node-note", `Observed ${time(snapshot.at)}`));
    if (live.error) node.append(element("p", "node-error", `Liveness: ${problem(live.error)}.`));
    return node;
  }
  function show(observations, fetchFailed = false) {
    nodes.replaceChildren(...observations.map(observation => card(observation, fetchFailed)));
    count.textContent = `${observations.length} ${observations.length === 1 ? "node" : "nodes"}`;
  }
  let latest = [];
  async function refresh() {
    if (busy) return;
    busy = true;
    clearTimeout(timer);
    refreshButton.disabled = true;
    try {
      const response = await fetch("/api/status", { cache: "no-store" });
      if (!response.ok) throw new Error("status request failed");
      const data = await response.json();
      if (!Array.isArray(data.nodes)) throw new Error("invalid response");
      latest = data.nodes;
      show(latest);
      message.textContent = latest.length ? "" : "No nodes are configured. Add nodes to the dashboard configuration.";
      lastRefresh.textContent = `Refreshed ${time(new Date())}`;
    } catch {
      message.textContent = "Could not refresh the dashboard. Check that the local dashboard server is running, then try again.";
      if (latest.length) show(latest.map(item => ({ id: item.id, liveness: { error: "unavailable" }, readiness: { error: "unavailable" } })), true);
      lastRefresh.textContent = "Refresh failed";
    } finally {
      busy = false;
      refreshButton.disabled = false;
      if (!document.hidden) timer = setTimeout(refresh, 10000);
    }
  }
  refreshButton.addEventListener("click", refresh);
  document.addEventListener("visibilitychange", () => {
    clearTimeout(timer);
    if (!document.hidden) refresh();
  });
  refresh();
})();
