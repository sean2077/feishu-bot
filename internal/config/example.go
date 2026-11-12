package config

// Example is embedded in the binary so init works outside a source checkout.
const Example = `{
  "tenantId": "tenant_example",
  "appId": "cli_example_app",
  "appSecretEnv": "FEISHU_APP_SECRET",
  "dataDir": "./state",
  "runtime": { "maxWorkers": 4 },
  "model": {
    "baseUrl": "https://model.example/anthropic",
    "tokenEnv": "MODEL_AUTH_TOKEN",
    "model": "your-model-id",
    "smallModel": "your-model-id"
  },
  "modelProfiles": {},
  "projects": [{
    "id": "assistant",
    "maintainers": ["ou_example_owner"],
    "permissionMode": "acceptEdits",
    "actions": []
  }],
  "groups": [{
    "chatId": "oc_example_group",
    "projectId": "assistant",
    "members": ["ou_example_member"],
    "mentionOptional": false,
    "allowOtherBots": false,
    "otherBots": []
  }],
  "dms": [{ "userId": "ou_example_owner", "projectId": "assistant" }],
  "feedback": {
    "export": {
      "enabled": false,
      "dir": "./feedback-export"
    }
  },
  "limits": {
    "backgroundMessages": 0,
    "imageBatchMs": 0,
    "imageBatchMaxImages": 8,
    "attachmentBytes": 20971520,
    "imageBatchBytes": 41943040
  }
}
`
