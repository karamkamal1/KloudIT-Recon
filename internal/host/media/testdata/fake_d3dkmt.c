// Stand-in for gdi32's D3DKMTSetProcessSchedulingPriorityClass, for
// TestD3DKMTCall (Wine's gdi32 does not export it), and for the adapter
// queries of kernelHAGS (TestKernelHAGS). Build with mingw-w64:
//   x86_64-w64-mingw32-gcc -O2 -shared -o fake_d3dkmt.dll fake_d3dkmt.c
#include <windows.h>

static LONG refuse;  // 1: REALTIME refused, 2: REALTIME and HIGH refused
static LONG calls;
static int classes[8];
static DWORD pids[8];

// Returns NTSTATUS in the low 32 bits; the high bits are set on purpose
// (undefined for a 32-bit return value in the x64 ABI).
__declspec(dllexport) ULONGLONG D3DKMTSetProcessSchedulingPriorityClass(HANDLE process, int cls) {
	// GetProcessId needs PROCESS_QUERY_(LIMITED_)INFORMATION on the handle.
	DWORD pid = GetProcessId(process);
	LONG n = InterlockedIncrement(&calls);
	if (n <= 8) {
		classes[n - 1] = cls;
		pids[n - 1] = pid;
	}
	ULONGLONG junk = 0xA5A5A5A500000000ull;
	if (pid == 0) return junk | 0xC0000008u;              // STATUS_INVALID_HANDLE
	if (cls == 5 && refuse >= 1) return junk | 0xC0000061u; // STATUS_PRIVILEGE_NOT_HELD
	if (cls == 4 && refuse >= 2) return junk | 0xC0000022u; // STATUS_ACCESS_DENIED
	return junk;                                           // STATUS_SUCCESS
}

__declspec(dllexport) void FakeReset(LONG r) {
	refuse = r;
	calls = 0;
}

// FakeCall returns the class of call i (0-based) and stores its pid; -1 when
// there was no such call.
__declspec(dllexport) int FakeCall(LONG i, DWORD *pid) {
	if (i < 0 || i >= calls || i >= 8) return -1;
	*pid = pids[i];
	return classes[i];
}

// D3DKMTOpenAdapterFromLuid / D3DKMTQueryAdapterInfo / D3DKMTCloseAdapter as
// in d3dkmthk.h. FakeAdapter sets the D3DKMT_WDDM_2_7_CAPS value returned and
// which call fails (1: open, 2: query); FakeAdapterLog reports what was asked.
typedef struct { LUID luid; UINT adapter; } OPENADAPTERFROMLUID;
typedef struct { UINT adapter; int type; void *data; UINT size; } QUERYADAPTERINFO;

static UINT caps27, failAt;
static UINT adapterLog[8]; // opens, luid low, luid high, queries, adapter, type, size, closes of that adapter

__declspec(dllexport) ULONGLONG D3DKMTOpenAdapterFromLuid(OPENADAPTERFROMLUID *d) {
	adapterLog[0]++;
	adapterLog[1] = d->luid.LowPart;
	adapterLog[2] = (UINT)d->luid.HighPart;
	if (failAt == 1) return 0xA5A5A5A5C000000Dull; // STATUS_INVALID_PARAMETER
	d->adapter = 0x40000240;
	return 0xA5A5A5A500000000ull;
}

__declspec(dllexport) ULONGLONG D3DKMTQueryAdapterInfo(QUERYADAPTERINFO *q) {
	adapterLog[3]++;
	adapterLog[4] = q->adapter;
	adapterLog[5] = q->type;
	adapterLog[6] = q->size;
	if (failAt == 2 || q->type != 70 || q->size != sizeof(UINT)) return 0xA5A5A5A5C000000Dull;
	*(UINT *)q->data = caps27;
	return 0xA5A5A5A500000000ull;
}

__declspec(dllexport) ULONGLONG D3DKMTCloseAdapter(const UINT *adapter) {
	if (*adapter == 0x40000240) adapterLog[7]++;
	return 0xA5A5A5A500000000ull;
}

__declspec(dllexport) void FakeAdapter(UINT caps, UINT fail) {
	caps27 = caps;
	failAt = fail;
	for (int i = 0; i < 8; i++) adapterLog[i] = 0;
}

__declspec(dllexport) void FakeAdapterLog(UINT out[8]) {
	for (int i = 0; i < 8; i++) out[i] = adapterLog[i];
}
