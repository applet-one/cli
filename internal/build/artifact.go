package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	moduleContentType = "application/javascript+module"
	mainModule        = "modules/worker.mjs"
	backupPublicKey   = "MCowBQYDK2VwAyEAR0dMKypRJSk6yKF3oeXMvgDrIStBJpS+r+0ioZzmfU4="
)

// ManifestModule describes one module in the artifact.
type ManifestModule struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}

// ManifestBinding describes a binding in the artifact.
type ManifestBinding struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ClassName string `json:"class_name"`
}

// Manifest is the artifact v1 manifest.json.
type Manifest struct {
	FormatVersion      int               `json:"format_version"`
	CompatibilityDate  string            `json:"compatibility_date"`
	CompatibilityFlags []string          `json:"compatibility_flags,omitempty"`
	MainModule         string            `json:"main_module"`
	Modules            []ManifestModule  `json:"modules"`
	Bindings           []ManifestBinding `json:"bindings"`
	Vars               map[string]string `json:"vars,omitempty"`
	Migrations         []any             `json:"migrations"`
}

// Artifact is a packaged .applet.tgz.
type Artifact struct {
	Bytes    []byte
	Manifest Manifest
}

// SHA256 returns the hex digest of the artifact bytes.
func (a *Artifact) SHA256() string {
	sum := sha256.Sum256(a.Bytes)
	return hex.EncodeToString(sum[:])
}

type module struct {
	name    string
	content []byte
}

// Artifact packages the bundled Worker. appID is required when backups are
// enabled, because the backup helper only accepts tokens for that app.
func (r *Result) Artifact(appID string) (*Artifact, error) {
	modules, err := r.modules(appID)
	if err != nil {
		return nil, err
	}
	c := r.Config
	manifest := Manifest{
		FormatVersion:      1,
		CompatibilityDate:  c.CompatibilityDate,
		CompatibilityFlags: c.CompatibilityFlags,
		MainModule:         mainModule,
		Bindings:           []ManifestBinding{{Name: c.Binding.Name, Type: "durable_object_namespace", ClassName: c.Binding.ClassName}},
		Vars:               c.Vars,
		Migrations:         c.Migrations,
	}
	for _, m := range modules {
		sum := sha256.Sum256(m.content)
		manifest.Modules = append(manifest.Modules, ManifestModule{Name: m.name, ContentType: moduleContentType, SHA256: hex.EncodeToString(sum[:])})
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	// A zero gzip header ModTime and fixed tar metadata keep the artifact
	// reproducible.
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	entries := append([]module{{"manifest.json", append(manifestJSON, '\n')}}, modules...)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.content)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
		if err := archive.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := archive.Write(entry.content); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return &Artifact{Bytes: buffer.Bytes(), Manifest: manifest}, nil
}

func (r *Result) modules(appID string) ([]module, error) {
	c := r.Config
	if c.Backup == nil {
		return []module{{mainModule, r.Source}}, nil
	}
	if appID == "" {
		return nil, errors.New("Internal error: app id is required to create a backup-enabled artifact.")
	}
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	wrapper := strings.NewReplacer(
		"$CLASS", c.Binding.ClassName,
		"$APP_ID", quote(appID),
		"$PUBLIC_KEY", quote(backupPublicKey),
		"$BINDING", quote(c.Binding.Name),
		"$OBJECT_NAME", quote(c.Backup.ObjectName),
	).Replace(backupWrapper)
	return []module{{mainModule, []byte(wrapper)}, {"modules/user-worker.mjs", r.Source}}, nil
}

// backupWrapper serves a signed, size-limited state export at
// /.well-known/applet/backup and delegates every other request to user code.
const backupWrapper = `import userDefault, { $CLASS as UserObject } from "./user-worker.mjs";
const b64u=s=>Uint8Array.from(atob(s.replace(/-/g,"+").replace(/_/g,"/")),c=>c.charCodeAt(0));
const json=s=>JSON.parse(new TextDecoder().decode(b64u(s)));
const value=v=>v instanceof ArrayBuffer?{__applet_blob__:btoa(String.fromCharCode(...new Uint8Array(v)))}:v;
async function authorized(request){const token=request.headers.get("authorization")?.replace(/^Bearer /,"");if(!token)return false;const [h,p,s,...rest]=token.split(".");if(rest.length||!h||!p||!s)return false;try{const claims=json(p);if(claims.app_id!==$APP_ID||claims.action!=="backup"||!Number.isSafeInteger(claims.exp)||claims.exp<Math.floor(Date.now()/1000))return false;const key=await crypto.subtle.importKey("spki",b64u($PUBLIC_KEY),{name:"Ed25519"},false,["verify"]);return crypto.subtle.verify("Ed25519",key,b64u(s),new TextEncoder().encode(h+"."+p));}catch{return false}}
export class $CLASS extends UserObject { constructor(ctx,env){super(ctx,env);this.ctx=ctx} async fetch(request){if(new URL(request.url).pathname!=="/.well-known/applet/backup")return super.fetch(request);if(!await authorized(request))return new Response("Not found",{status:404});const tables=this.ctx.storage.sql.exec("SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='__cf_kv'").toArray();const sql=tables.map(table=>({...table,rows:this.ctx.storage.sql.exec('SELECT * FROM "'+table.name.replaceAll('"','""')+'"').toArray().map(row=>Object.fromEntries(Object.entries(row).map(([key,val])=>[key,value(val)])))}));const kv=[...this.ctx.storage.kv.list()].map(([key,val])=>[key,value(val)]);const body=JSON.stringify({format_version:1,created_at:new Date().toISOString(),sql,kv});if(new TextEncoder().encode(body).byteLength>10*1024*1024)return new Response("Backup exceeds the 10 MiB MVP limit.",{status:413});return new Response(body,{headers:{"content-type":"application/json","content-disposition":"attachment; filename=applet-backup.json"}})}}
export default {fetch(request,env,ctx){if(new URL(request.url).pathname==="/.well-known/applet/backup")return env[$BINDING].get(env[$BINDING].idFromName($OBJECT_NAME)).fetch(request);return userDefault.fetch(request,env,ctx)}};`
