var assert = require("assert");

var codePath = "bin/counter-c.wasm";

var lang = "c"
var type = "wasm"
function deploy() {
    return xchain.Deploy({
        name: "counter",
        code: codePath,
        lang: lang,
        type: type,
        init_args: { "creator": "xchain" }
    });
}

Test("Increase", function (t) {
    var c = deploy();
    var resp = c.Invoke("increase", { "key": "xchain" }, { "name": "11111" });
    assert.equal(resp.Body, "1");
})

Test("Get", function (t) {
    var c = deploy()
    c.Invoke("Increase", { "key": "xchain" });
    var resp = c.Invoke("iet", { "key": "xchain" })
    assert.equal(resp.Body, "1")
})