package mcpserver

// This file carries the pre-connection discovery document for the hosted server:
// the MCP Server Card described by the experimental extension tracking SEP-2127.
//
// A card is served before a client speaks MCP, so it may only state what a client
// needs to find and connect: identity, transport and the protocol versions the
// SDK accepts. It deliberately does not list tools. The extension reserves those
// for the protocol's own tools/list, and a card that copied them would be a second
// description of the tool set that nothing keeps in step with the first.

// CardSchemaURL is the Server Card schema the document advertises. It is the only
// accepted value while the extension is at v1, and the schema itself requires it.
const CardSchemaURL = "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json"

// CardName is the server's reverse-DNS identity. It is the same name the registry
// listing publishes, because a card and a listing that named different servers
// would send a client looking for one that is not there.
const CardName = "io.github.douglasdemaio/vtessera"

// CardDescription is what the server is for, in the card's hundred-character
// budget. It names the marketplace rather than this transport, because that is
// what a discovering client is deciding whether to talk to.
const CardDescription = "Discover, vet and route agent services on the vtessera A2A marketplace."

// CardWebsite is where a discovering client is sent to learn more.
const CardWebsite = "https://vtessera.fly.dev"

const (
	// CardRepositorySource and CardRepositoryURL point a security reviewer at the
	// code behind this server, which is the transparency the card's repository
	// field exists for.
	CardRepositorySource    = "github"
	CardRepositoryURL       = "https://github.com/douglasdemaio/vtessera"
	CardRepositorySubfolder = "mcp"
)

// ServerCard is the discovery document. Only the fields this server fills are
// modelled; the schema leaves the object open, and the ones left out are optional.
type ServerCard struct {
	Schema      string      `json:"$schema"`
	Name        string      `json:"name"`
	Title       string      `json:"title,omitempty"`
	Version     string      `json:"version"`
	Description string      `json:"description"`
	WebsiteURL  string      `json:"websiteUrl,omitempty"`
	Repository  *Repository `json:"repository,omitempty"`
	Remotes     []Remote    `json:"remotes,omitempty"`
}

// Repository identifies the source of this server's code.
type Repository struct {
	Source    string `json:"source"`
	URL       string `json:"url"`
	Subfolder string `json:"subfolder,omitempty"`
}

// Remote is one HTTP endpoint a client can connect to.
//
// supportedProtocolVersions is left unset on purpose. Only the SDK knows which
// versions it accepts, and it does not export them: a hard-coded list here could
// disagree with the binary that answers initialize, which is the same defect the
// registry listing's version test exists to catch.
type Remote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Card builds the discovery document. remoteURL is the streamable-http endpoint
// this server is reachable at, which only the caller knows: a hosted server sits
// behind a proxy and cannot read its own public address from its listener.
func (s *Server) Card(remoteURL string) ServerCard {
	return ServerCard{
		Schema:      CardSchemaURL,
		Name:        CardName,
		Title:       "vtessera agent marketplace",
		Version:     Version,
		Description: CardDescription,
		WebsiteURL:  CardWebsite,
		Repository: &Repository{
			Source:    CardRepositorySource,
			URL:       CardRepositoryURL,
			Subfolder: CardRepositorySubfolder,
		},
		Remotes: []Remote{{Type: "streamable-http", URL: remoteURL}},
	}
}
