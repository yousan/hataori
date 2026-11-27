"""Normalize identifying transport metadata without rewriting agent evidence."""


def normalize_metadata(value):
    if isinstance(value, list):
        return [normalize_metadata(item) for item in value]
    if not isinstance(value, dict):
        return value
    result = {key: normalize_metadata(item) for key, item in value.items()}
    if result.get("method") == "remoteControl/status/changed":
        params = result.get("params")
        if isinstance(params, dict) and isinstance(params.get("serverName"), str):
            if params["serverName"]:
                params["serverName"] = "fixture-server"
    return result
