import test from "node:test";
import assert from "node:assert/strict";
import { httpErrorMessage, connectionErrorMessage, nativeFailureMessage } from "./error-message.mjs";

test("HTTP failures name the operation and avoid upstream HTML or opaque runtime output", () => {
  for (const [status, reason] of [[401, "登录会话"], [409, "操作"], [413, "大小"], [503, "尚未就绪"], [504, "超时"]]) {
    const message = httpErrorMessage("/environment/resources", status, "<html>token=private</html>", "text/html");
    assert.match(message, /资源配置/);
    assert.ok(message.includes(reason));
    assert.ok(!message.includes("private"));
  }
  const specific = "上传窗口已关闭，请重新点击网页上传按钮";
  assert.equal(httpErrorMessage("/input/files", 409, specific, "text/plain; charset=utf-8"), specific);
  assert.match(connectionErrorMessage("/input/files"), /传递文件.*连接已中断.*核对结果/);
  assert.match(httpErrorMessage("/settings", 503, "", "", "POST"), /无法确认设置是否已保存.*核对设置/);
});

test("native errors retain precise rules and explain opaque failures in context", () => {
  assert.match(nativeFailureMessage("exportDiagnostics", "Native operation timeout"), /导出诊断包.*超时/);
  assert.match(nativeFailureMessage("uploadHostFiles", "operation failed token=private"), /上传本机文件失败.*可识别的原因/);
  assert.ok(!nativeFailureMessage("uploadHostFiles", "operation failed token=private").includes("private"));
  const rule = "请选择本机项目中的文件";
  assert.equal(nativeFailureMessage("uploadHostFiles", rule), rule);
});
