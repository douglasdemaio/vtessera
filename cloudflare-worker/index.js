// Cloudflare Worker: marketplace registration proxy
//
// Accepts POST /register and POST /deregister from vtessera nodes.
// Forwards to GitHub repository_dispatch on the upstream repo.
// Nodes don't need a GitHub token — the worker holds it.

export default {
  async fetch(request, env) {
    if (request.method === "OPTIONS") {
      return cors(null, 204);
    }

    if (request.method !== "POST") {
      return cors({ error: "method not allowed" }, 405);
    }

    const url = new URL(request.url);
    let action;

    if (url.pathname === "/register") {
      action = "node-register";
    } else if (url.pathname === "/deregister") {
      action = "node-deregister";
    } else if (url.pathname === "/health") {
      return cors({ status: "ok" });
    } else {
      return cors({ error: "not found" }, 404);
    }

    let body;
    try {
      body = await request.json();
    } catch {
      return cors({ error: "invalid json" }, 400);
    }

    // Forward to GitHub repository_dispatch
    const ghUrl = `https://api.github.com/repos/douglasdemaio/vtessera/dispatches`;
    const ghResp = await fetch(ghUrl, {
      method: "POST",
      headers: {
        "Authorization": `Bearer ${env.GITHUB_TOKEN}`,
        "Accept": "application/vnd.github+json",
        "Content-Type": "application/json",
        "X-GitHub-Api-Version": "2022-11-28",
      },
      body: JSON.stringify({
        event_type: action,
        client_payload: body,
      }),
    });

    if (ghResp.status === 204) {
      return cors({ status: "ok", action });
    } else {
      const text = await ghResp.text();
      return cors({ error: `github ${ghResp.status}`, detail: text }, 502);
    }
  },
};

function cors(body, status = 200) {
  return new Response(body ? JSON.stringify(body) : null, {
    status,
    headers: {
      "Content-Type": "application/json",
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "POST, OPTIONS",
      "Access-Control-Allow-Headers": "Content-Type",
    },
  });
}
