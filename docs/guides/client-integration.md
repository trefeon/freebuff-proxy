# Client Integration Guide

Connect any OpenAI-compatible AI tool or coding assistant to `freebuff-proxy`.

**Server Base URL:** `http://localhost:3457/v1`  
**API Key:** `not-needed` (or your FreeBuff token in bridge mode)

---

## 1. Continue (VS Code & JetBrains Extension)

Edit `~/.continue/config.json`:

```json
{
  "models": [
    {
      "title": "FreeBuff DeepSeek Flash",
      "provider": "openai",
      "model": "deepseek/deepseek-v4-flash",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    },
    {
      "title": "FreeBuff GLM 5.2",
      "provider": "openai",
      "model": "z-ai/glm-5.2",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    }
  ]
}
```

---

## 2. Cursor IDE

1. Open **Cursor Settings** -> **Models**
2. Scroll to **OpenAI API Key** or **Custom OpenAI Compatible Provider**
3. Override **Base URL**: `http://localhost:3457/v1`
4. Enter **API Key**: `not-needed`
5. Add Model: `deepseek/deepseek-v4-flash`

---

## 3. aider (CLI AI Pair Programmer)

Run `aider` with environment variables or CLI flags:

```bash
export OPENAI_API_BASE="http://localhost:3457/v1"
export OPENAI_API_KEY="not-needed"

aider --model openai/deepseek/deepseek-v4-flash
```

---

## 4. opencode

Add to `opencode.json` or `~/.config/opencode/opencode.json`:

```json
{
  "providers": {
    "freebuff": {
      "type": "openai",
      "options": {
        "baseURL": "http://localhost:3457/v1",
        "apiKey": "not-needed"
      },
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" },
        { "id": "z-ai/glm-5.2", "name": "GLM 5.2" }
      ]
    }
  }
}
```

---

## 5. Python (Official OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3457/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "Write a python function to check prime numbers."}],
    stream=True
)

for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

---

## 6. Node.js (Official OpenAI SDK)

```javascript
import OpenAI from 'openai';

const openai = new OpenAI({
  baseURL: 'http://localhost:3457/v1',
  apiKey: 'not-needed',
});

const response = await openai.chat.completions.create({
  model: 'deepseek/deepseek-v4-flash',
  messages: [{ role: 'user', content: 'Say hello in TypeScript!' }],
  stream: true,
});

for await (const chunk of response) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

---

## Recommended Models

Query `http://localhost:3457/v1/models` for the full live list.

| Model ID | Provider | Best for |
|---|---|---|
| `deepseek/deepseek-v4-flash` | DeepSeek | Fast coding, auto-complete, inline refactors |
| `z-ai/glm-5.2` | Zhipu AI | Code explanation, architecture, complex reasoning |
| `meta-llama/llama-3.3-70b-instruct` | Meta | General instructions & conversation |
