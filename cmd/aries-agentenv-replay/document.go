package main

// Read the actual file even on the recorded missing-file path. The exit status
// and Node's native error message come from the current guest filesystem.
const missingFileRead = `
const fs = require("fs"), path = process.argv[1];
try {
  fs.accessSync(path);
  process.stdout.write(fs.readFileSync(path));
} catch (e) {
  console.log(JSON.stringify({status:"error", tool:"read", error:e.message}, null, 2));
  process.exitCode = 1;
}`

const documentOperation = `
const fs = require("fs");
const [op, raw] = process.argv.slice(1);
const {path, content, oldText, newText} = JSON.parse(raw);
if (op === "WriteFile") {
  fs.mkdirSync(require("path").dirname(path), {recursive:true});
  fs.writeFileSync(path, content, "utf8");
  console.log("Successfully wrote " + content.length + " bytes to " + path);
} else {
  const text = fs.readFileSync(path, "utf8");
  const start = text.indexOf(oldText);
  if (start < 0 || text.indexOf(oldText, start + 1) !== -1) throw Error("edit must match once");
  fs.writeFileSync(path, text.slice(0,start) + newText + text.slice(start + oldText.length), "utf8");
  console.log("Successfully replaced text in " + path + ".");
}`
