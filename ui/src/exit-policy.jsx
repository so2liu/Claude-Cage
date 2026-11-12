import { InlineSelect } from "./inline-select";
import React from "react";
import { Globe2, LoaderCircle } from "lucide-react";
import { canonicalExitPolicy } from "./safety-state.mjs";

const countries =
  "US GB CA DE FR NL SE FI IE CH AU NZ JP SG KR HK MO TW IN BR".split(" ");
const names = new Intl.DisplayNames(["zh-CN"], { type: "region" });

export function ExitPolicyFields({
  value,
  onChange,
  observation,
  onProbe,
  busy,
  probing,
}) {
  const policy = { ...canonicalExitPolicy(), ...value };
  const update = (key, next) => onChange({ ...policy, [key]: next });
  const bind = () =>
    onChange(
      policy.mode === "static"
        ? { ...policy, ip: observation.ip }
        : {
            ...policy,
            country_code: observation.country_code || "",
            ...(policy.scope === "city" || policy.scope === "region"
              ? {
                  region: observation.region || "",
                  ...(policy.scope === "city" ? { city: observation.city || "" } : {}),
                }
              : {}),
          },
    );
  return (
    <>
      <div className="section-title">
        <h3>IP 变化规则</h3>
        <p>中国大陆出口禁止放行；静态 IP 和不限地区也必须确认出口国家。</p>
      </div>
      <label className="toggle-row">
        <span>
          <strong>静态 IP</strong>
          <small>绑定当前最终出口节点；同一节点的 IPv4 地址变化时暂停外网。</small>
        </span>
        <input
          type="checkbox"
          role="switch"
          checked={policy.mode === "static"}
          onChange={(e) =>
            update("mode", e.target.checked ? "static" : "dynamic")
          }
        />
      </label>
      {policy.mode === "static" ? (
        <label>
          绑定的 IPv4 地址
          <input
            value={policy.ip}
            placeholder="首次使用此节点时自动检测并绑定"
            spellCheck="false"
            onChange={(e) => update("ip", e.target.value)}
          />
        </label>
      ) : (
        <>
          <label>
            动态 IP 的允许范围
            <InlineSelect
              aria-label="动态 IP 的允许范围"
              value={policy.scope}
              onValueChange={(next) => update("scope", next)}
            >
              <option value="city">同一城市</option>
              <option value="region">同一州／省</option>
              <option value="country">同一国家／地区</option>
              <option value="any">不限地区（中国大陆除外）</option>
            </InlineSelect>
          </label>
          {policy.scope !== "any" && (
            <>
              <label>
                允许的国家／地区代码
                <InlineSelect editable
                  aria-label="允许的国家／地区代码"
                  value={policy.country_code}
                  onValueChange={(next) =>
                    update("country_code", next.toUpperCase())
                  }
                  placeholder="例如 US；留空则在首次保存时绑定"
                  maxLength={2}
                  spellCheck="false"
                >
                  {countries.map((code) => (
                    <option key={code} value={code}>
                      {names.of(code)}
                    </option>
                  ))}
                </InlineSelect>
              </label>
              {(policy.scope === "city" || policy.scope === "region") && (
                <div className="field-row">
                  <label>
                    所属州／省
                    <input
                      value={policy.region}
                      onChange={(e) => update("region", e.target.value)}
                      placeholder="例如 California"
                    />
                  </label>
                  {policy.scope === "city" && (
                    <label>
                      允许的城市
                      <input
                        value={policy.city}
                        onChange={(e) => update("city", e.target.value)}
                        placeholder="例如 Los Angeles"
                      />
                    </label>
                  )}
                </div>
              )}
              <p className="field-help">
                {policy.scope === "city"
                  ? "城市同时匹配所属国家和州／省。名称以定位结果为准，建议使用检测结果填写。"
                  : policy.scope === "region"
                    ? "州／省同时匹配所属国家，包括日本的都道府县。名称以检测结果为准。"
                    : "建议先检测出口，再使用检测到的国家／地区填写。"}
              </p>
            </>
          )}
        </>
      )}
      <div className="probe-line">
        <button disabled={busy} onClick={onProbe}>
          {probing ? (
            <LoaderCircle size={15} className="spin" />
          ) : (
            <Globe2 size={15} />
          )}
          检测当前出口
        </button>
        <button
          disabled={
            busy ||
            !observation ||
            (policy.mode === "dynamic" && policy.scope === "any")
          }
          onClick={bind}
        >
          用检测结果填写
        </button>
      </div>
      {observation && (
        <div className="exit-observation">
          <strong>
            {observation.country} · {observation.region} · {observation.city}
          </strong>
          <code>{observation.ip}</code>
          <small>
            {observation.country_code} ·{" "}
            {new Date(observation.checked_at).toLocaleTimeString("zh-CN")}
          </small>
        </div>
      )}
      <p className="footnote">
        出口规则按最终出口节点保存，切换节点时自动恢复；首次使用的节点检测后绑定。仅更换中转、重检或重启会保留该节点的基准。命中大陆网段、定位为中国大陆、无法确认出口地区或超出保存范围时暂停外网，离线终端和文件仍可使用。
      </p>
      <p className="field-help">
        范围依据 IP 数据库的近似定位。后台约每 30
        秒采样，不能代替代理自身的分流约束。
      </p>
    </>
  );
}
