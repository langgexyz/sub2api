"""cc-replay MCP server 单测：覆盖参数路由与 data 提取（不依赖 live infra）。"""

import server


def test_user_param_numeric_is_user_id():
    assert server._user_param("11") == {"user_id": "11"}
    assert server._user_param(" 11 ") == {"user_id": "11"}


def test_user_param_name_is_username():
    assert server._user_param("St-coder") == {"username": "St-coder"}


class _FakeResp:
    def __init__(self, payload):
        self._payload = payload

    def raise_for_status(self):
        pass

    def json(self):
        return self._payload


class _FakeClient:
    def __init__(self, payload):
        self._payload = payload
        self.last = None

    def get(self, path, params):
        self.last = (path, params)
        return _FakeResp(self._payload)


def test_get_extracts_data_and_drops_empty_params(monkeypatch):
    fake = _FakeClient({"code": 0, "message": "success", "data": [{"session_hash": "abc"}]})
    monkeypatch.setattr(server, "_http", lambda: fake)
    out = server._get("/x", {"a": "1", "b": "", "c": None})
    assert out == [{"session_hash": "abc"}]
    # 空值参数被剔除
    assert fake.last == ("/x", {"a": "1"})


def test_get_raises_on_nonzero_code(monkeypatch):
    fake = _FakeClient({"code": 1, "message": "boom"})
    monkeypatch.setattr(server, "_http", lambda: fake)
    try:
        server._get("/x", {})
    except RuntimeError as e:
        assert "boom" in str(e)
    else:
        raise AssertionError("应抛出非 0 code 错误")


def test_list_user_sessions_routes_username(monkeypatch):
    fake = _FakeClient({"code": 0, "data": []})
    monkeypatch.setattr(server, "_http", lambda: fake)
    server.list_user_sessions("St-coder", limit=10)
    path, params = fake.last
    assert path == server.API
    assert params["username"] == "St-coder"
    assert params["limit"] == 10
