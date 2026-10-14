const [path, payload] = process.argv.slice(2);
let response;
for (let attempt = 0; ; attempt++) {
  try {
    response = await fetch(`http://127.0.0.1:8080${path}`, {
      method: payload ? 'POST' : 'GET', headers: { 'Content-Type': 'application/json' }, body: payload,
      signal: AbortSignal.timeout(10000),
    });
    break;
  } catch (error) {
    if (path !== '/health' || attempt >= 49) throw error;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}
if (!response.ok) throw new Error(`control request failed: ${response.status}`);
process.stdout.write(await response.text());
