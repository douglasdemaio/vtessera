// Vtessera Marketplace Registration Worker
//
// Accepts POST requests from vtessera nodes and registers them
// with the GitHub Pages marketplace via repository_dispatch.
//
// No authentication required from the node side — the GitHub token
// is stored as a Cloudflare Worker secret.
//
// Endpoints:
//   POST /register  — register a node (JSON body: { offer, sig_hex })
//   POST /deregister — deregister a node (JSON body: { node_id })
//   GET  /health    — health check

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // CORS headers for browser access
    const corsHeaders = {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
    };

    // Handle preflight
    if (request.method === 'OPTIONS') {
      return new Response(null, { headers: corsHeaders });
    }

    // Health check
    if (url.pathname === '/health' && request.method === 'GET') {
      return new Response(JSON.stringify({ status: 'ok' }), {
        headers: { ...corsHeaders, 'Content-Type': 'application/json' },
      });
    }

    // Registration
    if (url.pathname === '/register' && request.method === 'POST') {
      return handleRegister(request, env, corsHeaders);
    }

    // Deregistration
    if (url.pathname === '/deregister' && request.method === 'POST') {
      return handleDeregister(request, env, corsHeaders);
    }

    return new Response('Not Found', { status: 404, headers: corsHeaders });
  },
};

async function handleRegister(request, env, corsHeaders) {
  try {
    const body = await request.json();

    // Validate required fields
    if (!body.offer || !body.sig_hex) {
      return new Response(
        JSON.stringify({ error: 'missing offer or sig_hex' }),
        { status: 400, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }

    const nodeId = body.offer?.body?.node_id;
    if (!nodeId) {
      return new Response(
        JSON.stringify({ error: 'missing node_id in offer' }),
        { status: 400, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }

    // Forward to GitHub repository_dispatch
    const githubToken = env.GITHUB_TOKEN;
    if (!githubToken) {
      return new Response(
        JSON.stringify({ error: 'worker not configured (no GitHub token)' }),
        { status: 500, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }

    const payload = {
      event_type: 'node-register',
      client_payload: {
        offer: body.offer,
        sig_hex: body.sig_hex,
      },
    };

    const resp = await fetch(
      `https://api.github.com/repos/${env.GITHUB_OWNER}/${env.GITHUB_REPO}/dispatches`,
      {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${githubToken}`,
          'Accept': 'application/vnd.github+json',
          'X-GitHub-Api-Version': '2022-11-28',
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(payload),
      }
    );

    if (resp.status === 204) {
      return new Response(
        JSON.stringify({ status: 'registered', node_id: nodeId }),
        { status: 200, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    } else {
      const text = await resp.text();
      return new Response(
        JSON.stringify({ error: `github error: ${resp.status}`, detail: text }),
        { status: 502, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }
  } catch (e) {
    return new Response(
      JSON.stringify({ error: e.message }),
      { status: 500, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
    );
  }
}

async function handleDeregister(request, env, corsHeaders) {
  try {
    const body = await request.json();

    if (!body.node_id) {
      return new Response(
        JSON.stringify({ error: 'missing node_id' }),
        { status: 400, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }

    const githubToken = env.GITHUB_TOKEN;
    if (!githubToken) {
      return new Response(
        JSON.stringify({ error: 'worker not configured' }),
        { status: 500, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }

    const payload = {
      event_type: 'node-deregister',
      client_payload: { node_id: body.node_id },
    };

    const resp = await fetch(
      `https://api.github.com/repos/${env.GITHUB_OWNER}/${env.GITHUB_REPO}/dispatches`,
      {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${githubToken}`,
          'Accept': 'application/vnd.github+json',
          'X-GitHub-Api-Version': '2022-11-28',
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(payload),
      }
    );

    if (resp.status === 204) {
      return new Response(
        JSON.stringify({ status: 'deregistered', node_id: body.node_id }),
        { status: 200, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    } else {
      const text = await resp.text();
      return new Response(
        JSON.stringify({ error: `github error: ${resp.status}`, detail: text }),
        { status: 502, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
      );
    }
  } catch (e) {
    return new Response(
      JSON.stringify({ error: e.message }),
      { status: 500, headers: { ...corsHeaders, 'Content-Type': 'application/json' } }
    );
  }
}
