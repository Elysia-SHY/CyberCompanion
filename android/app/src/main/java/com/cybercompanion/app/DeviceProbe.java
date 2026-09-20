package com.cybercompanion.app;

import android.Manifest;
import android.app.ActivityManager;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.net.ConnectivityManager;
import android.net.LinkAddress;
import android.net.LinkProperties;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.TrafficStats;
import android.net.TransportInfo;
import android.net.wifi.WifiInfo;
import android.net.wifi.WifiManager;
import android.os.BatteryManager;
import android.os.Build;
import android.os.PowerManager;
import android.os.StatFs;
import android.os.SystemClock;
import android.telephony.CellSignalStrength;
import android.telephony.SignalStrength;
import android.telephony.TelephonyManager;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStreamReader;
import java.net.InetAddress;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.Locale;

/**
 * Android 框架层设备信息采集器。
 *
 * <p>为什么需要它：Go 核心是作为应用沙箱子进程跑的，Android 10 之后
 * SELinux 把 {@code /proc/net/*}、{@code /proc/loadavg}、{@code /proc/uptime}、
 * {@code /sys/class/net}、{@code /sys/class/thermal}、{@code /sys/class/power_supply}
 * 这些路径对普通应用全部关掉了，于是面板上「安卓大部分都读不到」。
 *
 * <p>这些数据其实都能从框架 API 拿到（Build / ActivityManager / StatFs /
 * BatteryManager / PowerManager / ConnectivityManager / TelephonyManager /
 * TrafficStats / SystemClock），只是 Go 侧没有权限调用系统服务。
 * 因此这里把采集结果写成一份 JSON 快照，由 {@link BotService} 通过环境变量
 * {@link #SNAPSHOT_ENV} 把路径交给核心子进程，核心优先采用快照值。
 *
 * <p>字段名必须与 {@code internal/hal/android_snapshot.go} 里的 struct tag 保持一致，
 * 改一边就要同步改另一边。
 */
public final class DeviceProbe {

    private static final String TAG = "CyberCompanionProbe";

    /** 传给核心子进程的环境变量名（与 Go 侧 androidSnapshotEnv 一致）。 */
    public static final String SNAPSHOT_ENV = "CYBERCOMPANION_DEVICE_INFO";
    /** 快照文件名，落在应用私有目录里。 */
    public static final String SNAPSHOT_FILE = "device_info.json";
    /** 快照来源标记，Go 侧据此认领文件，避免误读别的 JSON。 */
    private static final String SNAPSHOT_SOURCE = "android-shell";
    /** 刷新间隔：电池、流量、网络类型都会变，需要定期重写。 */
    public static final long REFRESH_INTERVAL_MS = 15_000L;

    private DeviceProbe() {
    }

    // =======================================================================
    // 对外入口
    // =======================================================================

    /**
     * 采集一次并原子写入 {@code dest}。
     *
     * <p>任何异常都在内部吞掉：采集失败最多让面板少几个数字，
     * 绝不能因此把前台服务拖崩。
     *
     * @return 写入是否成功
     */
    public static boolean writeSnapshot(Context ctx, File dest) {
        if (ctx == null || dest == null) {
            return false;
        }
        try {
            JSONObject snapshot = collect(ctx);
            byte[] payload = snapshot.toString().getBytes(StandardCharsets.UTF_8);

            // 先写临时文件再 rename：核心侧是按 mtime/size 判断更新的，
            // 直接覆写有可能被读到写了一半的 JSON。
            File tmp = new File(dest.getAbsolutePath() + ".tmp");
            try (FileOutputStream fos = new FileOutputStream(tmp)) {
                fos.write(payload);
                fos.flush();
                fos.getFD().sync();
            }
            if (!tmp.renameTo(dest)) {
                // 个别 ROM 的 rename 会失败，退化成直接写，保证至少有数据
                try (FileOutputStream fos = new FileOutputStream(dest)) {
                    fos.write(payload);
                }
                //noinspection ResultOfMethodCallIgnored
                tmp.delete();
            }
            return true;
        } catch (Throwable e) {
            Log.w(TAG, "写入设备快照失败: " + e.getMessage());
            return false;
        }
    }

    // =======================================================================
    // 采集
    // =======================================================================

    private static JSONObject collect(Context ctx) throws JSONException {
        JSONObject o = new JSONObject();
        JSONArray unavailable = new JSONArray();

        o.put("source", SNAPSHOT_SOURCE);
        o.put("generated_at", new SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ssZ", Locale.US)
                .format(new Date()));

        collectBuild(o);
        collectCpu(ctx, o);
        collectMemory(ctx, o);
        collectStorage(ctx, o);
        collectSystem(o);
        collectPower(ctx, o);
        collectNetwork(ctx, o, unavailable);

        o.put("unavailable", unavailable);
        return o;
    }

    /** Build.* 是安卓上最权威的设备标识来源，且完全不需要权限。 */
    private static void collectBuild(JSONObject o) throws JSONException {
        o.put("manufacturer", safe(Build.MANUFACTURER));
        o.put("brand", safe(Build.BRAND));
        o.put("model", safe(Build.MODEL));
        o.put("product", safe(Build.PRODUCT));
        o.put("board", safe(Build.BOARD));
        o.put("hardware", safe(Build.HARDWARE));
        o.put("device_type", deviceName());
        o.put("android_release", safe(Build.VERSION.RELEASE));
        o.put("sdk_int", Build.VERSION.SDK_INT);

        String[] abis = Build.SUPPORTED_ABIS;
        JSONArray abiArr = new JSONArray();
        if (abis != null) {
            for (String abi : abis) {
                abiArr.put(abi);
            }
        }
        o.put("supported_abis", abiArr);
        o.put("cpu_abi", (abis != null && abis.length > 0) ? safe(abis[0]) : "");

        // SOC_MODEL / SOC_MANUFACTURER 是 API 31 才有的字段，
        // 低版本置空，由 Go 侧回退到 /proc/cpuinfo 或 Hardware。
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            o.put("soc_model", safe(Build.SOC_MODEL));
            o.put("soc_manufacturer", safe(Build.SOC_MANUFACTURER));
        } else {
            o.put("soc_model", "");
            o.put("soc_manufacturer", "");
        }
    }

    /** SoC 型号 / 核心数 / 主频 / 负载 / 调频策略 / 进程数。 */
    private static void collectCpu(Context ctx, JSONObject o) throws JSONException {
        o.put("cpu_cores", Runtime.getRuntime().availableProcessors());
        o.put("cpu_cur_mhz", readCpuCurFreqKhz() / 1000);
        o.put("cpu_max_mhz", readCpuMaxFreqKhz() / 1000);
        o.put("cpu_governor", safe(readFirstLine(
                "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")));
        o.put("load_avg", parseLoadAvg(readFirstLine("/proc/loadavg")));
        o.put("process_count", countProcesses());
    }

    /** 内存用量：ActivityManager.MemoryInfo 是安卓上的权威口径。 */
    private static void collectMemory(Context ctx, JSONObject o) throws JSONException {
        long totalMB = 0;
        long availMB = 0;
        boolean lowMemory = false;
        try {
            ActivityManager am = (ActivityManager) ctx.getSystemService(Context.ACTIVITY_SERVICE);
            if (am != null) {
                ActivityManager.MemoryInfo mi = new ActivityManager.MemoryInfo();
                am.getMemoryInfo(mi);
                totalMB = mi.totalMem / (1024L * 1024L);
                availMB = mi.availMem / (1024L * 1024L);
                lowMemory = mi.lowMemory;
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取内存信息失败: " + e.getMessage());
        }
        o.put("memory_total_mb", totalMB);
        o.put("memory_avail_mb", availMB);
        o.put("memory_used_mb", Math.max(0L, totalMB - availMB));
        o.put("low_memory", lowMemory);

        // /proc/meminfo 在安卓上仍可读，SwapTotal 反映的是 zram 容量
        long[] swap = readSwapFromMemInfo();
        o.put("swap_used_mb", swap[0]);
        o.put("swap_total_mb", swap[1]);
    }

    /** 存储：数据分区才是用户关心的那一块，StatFs("/") 读到的是只读 system 分区。 */
    private static void collectStorage(Context ctx, JSONObject o) throws JSONException {
        long totalMB = 0;
        long usedMB = 0;
        String mount = "";
        try {
            File dataDir = ctx.getFilesDir();
            if (dataDir != null) {
                StatFs fs = new StatFs(dataDir.getAbsolutePath());
                long total = fs.getTotalBytes();
                long avail = fs.getAvailableBytes();
                totalMB = total / (1024L * 1024L);
                usedMB = Math.max(0L, (total - avail)) / (1024L * 1024L);
                mount = "/data";
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取存储信息失败: " + e.getMessage());
        }
        o.put("storage_total_mb", totalMB);
        o.put("storage_used_mb", usedMB);
        o.put("storage_mount", mount);
    }

    /** 开机时长与内核版本。 */
    private static void collectSystem(JSONObject o) throws JSONException {
        o.put("uptime_sec", SystemClock.elapsedRealtime() / 1000L);
        o.put("kernel", safe(System.getProperty("os.version")));
        o.put("java_runtime", safe(System.getProperty("java.vm.version")));
    }

    /** 电池与散热。 */
    private static void collectPower(Context ctx, JSONObject o) throws JSONException {
        boolean present = false;
        int level = -1;
        String status = "";
        int tempC = 0;
        int voltageMv = 0;
        int currentUa = 0;
        boolean charging = false;
        String source = "";

        try {
            Intent batt = ctx.registerReceiver(null,
                    new IntentFilter(Intent.ACTION_BATTERY_CHANGED));
            if (batt != null) {
                int rawLevel = batt.getIntExtra(BatteryManager.EXTRA_LEVEL, -1);
                int scale = batt.getIntExtra(BatteryManager.EXTRA_SCALE, -1);
                if (rawLevel >= 0 && scale > 0) {
                    level = Math.round(rawLevel * 100f / scale);
                }
                int st = batt.getIntExtra(BatteryManager.EXTRA_STATUS, -1);
                status = batteryStatusLabel(st);
                charging = (st == BatteryManager.BATTERY_STATUS_CHARGING
                        || st == BatteryManager.BATTERY_STATUS_FULL);
                int tenths = batt.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, Integer.MIN_VALUE);
                if (tenths != Integer.MIN_VALUE && tenths > 0) {
                    tempC = Math.round(tenths / 10f);
                }
                voltageMv = batt.getIntExtra(BatteryManager.EXTRA_VOLTAGE, 0);
                source = powerSourceLabel(batt.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0));
            }

            BatteryManager bm = (BatteryManager) ctx.getSystemService(Context.BATTERY_SERVICE);
            if (bm != null) {
                if (level < 0) {
                    int cap = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY);
                    if (cap >= 0 && cap <= 100) {
                        level = cap;
                    }
                }
                int ua = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CURRENT_NOW);
                if (ua != Integer.MIN_VALUE) {
                    currentUa = ua;
                }
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取电池信息失败: " + e.getMessage());
        }

        present = level >= 0;
        if (!present) {
            level = -1;
        }
        o.put("battery_present", present);
        o.put("battery_level", level);
        o.put("battery_status", status);
        o.put("battery_temp_c", tempC);
        o.put("battery_voltage_mv", voltageMv);
        o.put("battery_current_ua", currentUa);
        o.put("charging", charging);
        o.put("power_source", source);

        int thermalStatus = -1;
        double headroom = -1d;
        try {
            PowerManager pm = (PowerManager) ctx.getSystemService(Context.POWER_SERVICE);
            if (pm != null) {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                    thermalStatus = pm.getCurrentThermalStatus();
                }
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    float h = pm.getThermalHeadroom(0);
                    if (!Float.isNaN(h) && h > 0f) {
                        headroom = h;
                    }
                }
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取散热状态失败: " + e.getMessage());
        }
        o.put("thermal_status", thermalStatus);
        o.put("thermal_headroom", headroom);
    }

    /** 网络类型 / 运营商 / 信号 / 地址 / 累计流量。 */
    private static void collectNetwork(Context ctx, JSONObject o, JSONArray unavailable)
            throws JSONException {
        String networkType = "";
        String subtype = "";
        String operator = "";
        int signalDbm = 0;   // 0 表示未知（dBm 恒为负数）
        int signalBars = 0;
        boolean vpn = false;
        boolean metered = false;
        JSONArray ips = new JSONArray();
        JSONArray dns = new JSONArray();

        boolean cellular = false;
        try {
            ConnectivityManager cm =
                    (ConnectivityManager) ctx.getSystemService(Context.CONNECTIVITY_SERVICE);
            Network net = cm != null ? cm.getActiveNetwork() : null;
            NetworkCapabilities caps = net != null ? cm.getNetworkCapabilities(net) : null;
            LinkProperties lp = net != null ? cm.getLinkProperties(net) : null;

            if (caps != null) {
                cellular = caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR);
                networkType = transportLabel(caps);
                vpn = caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN);
                metered = !caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED);
            }
            if (lp != null) {
                String iface = safe(lp.getInterfaceName());
                for (LinkAddress la : lp.getLinkAddresses()) {
                    InetAddress addr = la.getAddress();
                    if (addr == null || addr.isLoopbackAddress() || addr.isLinkLocalAddress()) {
                        continue;
                    }
                    String host = addr.getHostAddress();
                    if (host == null || host.isEmpty()) {
                        continue;
                    }
                    ips.put((iface.isEmpty() ? "net" : iface) + " " + host);
                }
                for (InetAddress server : lp.getDnsServers()) {
                    String host = server != null ? server.getHostAddress() : null;
                    if (host != null && !host.isEmpty()) {
                        dns.put(host);
                    }
                }
            }

            if (caps != null) {
                if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) {
                    int rssi = wifiRssi(caps, ctx);
                    if (rssi < 0) {
                        signalDbm = rssi;
                        signalBars = wifiBars(rssi);
                    }
                } else if (cellular) {
                    int[] sig = cellularSignal(ctx);
                    if (sig[0] != 0) {
                        signalDbm = sig[0];
                        signalBars = sig[1];
                    } else if (sig[1] > 0) {
                        // 拿不到 dBm 但拿得到等级：至少把格数显示出来
                        signalBars = sig[1];
                    }
                }
            }

            TelephonyManager tm =
                    (TelephonyManager) ctx.getSystemService(Context.TELEPHONY_SERVICE);
            if (tm != null) {
                operator = safe(tm.getNetworkOperatorName());
                if (cellular) {
                    subtype = radioLabel(tm);
                }
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取网络信息失败: " + e.getMessage());
        }

        // 蜂窝信号读不到时，说清楚是「缺权限」还是「ROM 不给」，
        // 免得用户以为程序坏了
        if (cellular && signalDbm == 0
                && !hasPermission(ctx, Manifest.permission.READ_PHONE_STATE)) {
            unavailable.put("蜂窝信号强度（未授予电话权限）");
        }
        // 安卓不给普通应用暴露 SoC 温度，能拿到的最接近指标是热状态与散热余量
        unavailable.put("SoC 温度（系统不向应用开放）");

        o.put("network_type", networkType);
        o.put("network_subtype", subtype);
        o.put("network_operator", operator);
        o.put("signal_dbm", signalDbm);
        o.put("signal_bars", signalBars);
        o.put("vpn", vpn);
        o.put("is_metered", metered);
        o.put("ip_addresses", ips);
        o.put("dns_servers", dns);

        long rx = 0;
        long tx = 0;
        try {
            long r = TrafficStats.getTotalRxBytes();
            long t = TrafficStats.getTotalTxBytes();
            if (r > 0) {
                rx = r;
            }
            if (t > 0) {
                tx = t;
            }
        } catch (Throwable ignored) {
        }
        o.put("rx_bytes", rx);
        o.put("tx_bytes", tx);
        o.put("mobile_rx_bytes", positiveOrZero(TrafficStats.getMobileRxBytes()));
        o.put("mobile_tx_bytes", positiveOrZero(TrafficStats.getMobileTxBytes()));
    }

    // =======================================================================
    // 工具方法
    // =======================================================================

    /** 拼出 "realme RMX6699" 这样的展示名。 */
    private static String deviceName() {
        String brand = safe(Build.BRAND);
        if (brand.isEmpty()) {
            brand = safe(Build.MANUFACTURER);
        }
        String model = safe(Build.MODEL);
        if (brand.isEmpty()) {
            return model.isEmpty() ? "Android 设备" : model;
        }
        if (model.isEmpty() || model.toLowerCase(Locale.US).startsWith(brand.toLowerCase(Locale.US))) {
            return model.isEmpty() ? brand : model;
        }
        return brand + " " + model;
    }

    private static String transportLabel(NetworkCapabilities caps) {
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) {
            return "Wi-Fi";
        }
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) {
            return "蜂窝网络";
        }
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) {
            return "有线以太网";
        }
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH)) {
            return "蓝牙共享";
        }
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) {
            return "VPN";
        }
        return "其他网络";
    }

    private static String radioLabel(TelephonyManager tm) {
        try {
            int type = tm.getDataNetworkType();
            switch (type) {
                case TelephonyManager.NETWORK_TYPE_NR:      // API 29，常量会被编译期内联
                    return "5G NR";
                case TelephonyManager.NETWORK_TYPE_LTE:
                    return "4G LTE";
                case TelephonyManager.NETWORK_TYPE_HSPAP:
                case TelephonyManager.NETWORK_TYPE_HSDPA:
                case TelephonyManager.NETWORK_TYPE_HSUPA:
                case TelephonyManager.NETWORK_TYPE_UMTS:
                case TelephonyManager.NETWORK_TYPE_EVDO_0:
                case TelephonyManager.NETWORK_TYPE_EVDO_A:
                    return "3G";
                case TelephonyManager.NETWORK_TYPE_EDGE:
                case TelephonyManager.NETWORK_TYPE_GPRS:
                case TelephonyManager.NETWORK_TYPE_CDMA:
                case TelephonyManager.NETWORK_TYPE_1xRTT:
                    return "2G";
                default:
                    return "";
            }
        } catch (Throwable e) {
            // 缺少 READ_PHONE_STATE 或其通信方式不允许多次调用时抛 SecurityException
            return "";
        }
    }

    /** Wi-Fi 信号强度；拿不到返回 0。 */
    private static int wifiRssi(NetworkCapabilities caps, Context ctx) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            try {
                TransportInfo info = caps.getTransportInfo();
                if (info instanceof WifiInfo) {
                    int rssi = ((WifiInfo) info).getRssi();
                    if (isUsableRssi(rssi)) {
                        return rssi;
                    }
                }
            } catch (Throwable ignored) {
            }
        }
        try {
            WifiManager wm = (WifiManager) ctx.getApplicationContext()
                    .getSystemService(Context.WIFI_SERVICE);
            WifiInfo info = wm != null ? wm.getConnectionInfo() : null;
            if (info != null && isUsableRssi(info.getRssi())) {
                return info.getRssi();
            }
        } catch (Throwable ignored) {
        }
        return 0;
    }

    private static boolean isUsableRssi(int rssi) {
        return rssi < 0 && rssi > -127;
    }

    private static int wifiBars(int rssi) {
        try {
            int level = WifiManager.calculateSignalLevel(rssi, 5);
            return Math.max(0, Math.min(5, level));
        } catch (Throwable e) {
            return 0;
        }
    }

    /**
     * 蜂窝信号，返回 {@code {dBm, 格数}}；dBm 为 0 表示拿不到精确强度。
     *
     * <p>需要 READ_PHONE_STATE。安卓把这些字段列为受保护项，
     * 没有权限时 {@link TelephonyManager#getSignalStrength()} 会抛 SecurityException。
     *
     * <p>注意 {@code SignalStrength.getDbm()} 已被移出公开 SDK
     * （API 30 起废弃、后来直接从 android.jar 里消失），
     * 所以只能用 API 30 的分制式接口 {@code getCellSignalStrengths()}；
     * API 28/29 退化成 0-4 的等级值。
     */
    private static int[] cellularSignal(Context ctx) {
        if (!hasPermission(ctx, Manifest.permission.READ_PHONE_STATE)) {
            return new int[]{0, 0};
        }
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.P) {
            return new int[]{0, 0};
        }
        try {
            TelephonyManager tm = (TelephonyManager) ctx.getSystemService(Context.TELEPHONY_SERVICE);
            if (tm == null) {
                return new int[]{0, 0};
            }
            SignalStrength ss = tm.getSignalStrength();
            if (ss == null) {
                return new int[]{0, 0};
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                for (CellSignalStrength cell : ss.getCellSignalStrengths()) {
                    int dbm = cell.getDbm();
                    if (isUsableDbm(dbm)) {
                        return new int[]{dbm, cellularBars(dbm)};
                    }
                }
            }
            int level = ss.getLevel();
            if (level > 0 && level <= 4) {
                return new int[]{0, Math.min(5, level + 1)};
            }
        } catch (Throwable e) {
            Log.w(TAG, "读取蜂窝信号失败: " + e.getMessage());
        }
        return new int[]{0, 0};
    }

    private static boolean isUsableDbm(int dbm) {
        return dbm < 0 && dbm > -140;
    }

    /** 把 -140..-40 dBm 粗略映射到 0..5 格。 */
    private static int cellularBars(int dbm) {
        if (dbm >= -75) {
            return 5;
        }
        if (dbm >= -85) {
            return 4;
        }
        if (dbm >= -95) {
            return 3;
        }
        if (dbm >= -105) {
            return 2;
        }
        if (dbm > -140) {
            return 1;
        }
        return 0;
    }

    private static String batteryStatusLabel(int status) {
        switch (status) {
            case BatteryManager.BATTERY_STATUS_CHARGING:
                return "充电中";
            case BatteryManager.BATTERY_STATUS_DISCHARGING:
                return "放电中";
            case BatteryManager.BATTERY_STATUS_FULL:
                return "已充满";
            case BatteryManager.BATTERY_STATUS_NOT_CHARGING:
                return "未充电";
            default:
                return "";
        }
    }

    private static String powerSourceLabel(int plugged) {
        switch (plugged) {
            case BatteryManager.BATTERY_PLUGGED_AC:
                return "电源适配器";
            case BatteryManager.BATTERY_PLUGGED_USB:
                return "USB";
            case BatteryManager.BATTERY_PLUGGED_WIRELESS:
                return "无线充电";
            default:
                return "电池";
        }
    }

    private static long positiveOrZero(long v) {
        return v > 0 ? v : 0L;
    }

    private static boolean hasPermission(Context ctx, String permission) {
        try {
            return ctx.checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED;
        } catch (Throwable e) {
            return false;
        }
    }

    private static String safe(String s) {
        return s == null ? "" : s.trim();
    }

    private static String readFirstLine(String path) {
        try (BufferedReader reader = new BufferedReader(
                new InputStreamReader(new FileInputStream(path), StandardCharsets.UTF_8))) {
            String line = reader.readLine();
            return line == null ? "" : line.trim();
        } catch (Throwable e) {
            // 安卓沙箱里这个文件大概率不可读，属于预期内的失败
            return "";
        }
    }

    private static String parseLoadAvg(String raw) {
        if (raw == null || raw.isEmpty()) {
            return "";
        }
        String[] fields = raw.split("\\s+");
        if (fields.length < 3) {
            return "";
        }
        return fields[0] + " / " + fields[1] + " / " + fields[2];
    }

    private static int countProcesses() {
        try {
            File proc = new File("/proc");
            String[] entries = proc.list();
            if (entries == null) {
                return 0;
            }
            int n = 0;
            for (String name : entries) {
                if (name.isEmpty()) {
                    continue;
                }
                boolean numeric = true;
                for (int i = 0; i < name.length(); i++) {
                    if (name.charAt(i) < '0' || name.charAt(i) > '9') {
                        numeric = false;
                        break;
                    }
                }
                if (numeric) {
                    n++;
                }
            }
            // 安卓沙箱里的 /proc 只暴露本应用自己的进程，数出来是个没意义的 1~2；
            // 这种情况按「拿不到」上报，胜过在面板上显示一个假数字。
            return n > 2 ? n : 0;
        } catch (Throwable e) {
            return 0;
        }
    }

    private static long readCpuCurFreqKhz() {
        for (String path : new String[]{
                "/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq",
                "/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_cur_freq"}) {
            long khz = parseLong(readFirstLine(path));
            if (khz > 0) {
                return khz;
            }
        }
        return 0;
    }

    private static long readCpuMaxFreqKhz() {
        long max = 0;
        for (int i = 0; i < 16; i++) {
            long khz = parseLong(readFirstLine(
                    "/sys/devices/system/cpu/cpu" + i + "/cpufreq/cpuinfo_max_freq"));
            if (khz > max) {
                max = khz;
            }
        }
        return max;
    }

    /** 从 /proc/meminfo 读 zram 交换分区用量，返回 {usedMB, totalMB}。 */
    private static long[] readSwapFromMemInfo() {
        long totalKb = 0;
        long freeKb = 0;
        try (BufferedReader reader = new BufferedReader(
                new InputStreamReader(new FileInputStream("/proc/meminfo"), StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                if (line.startsWith("SwapTotal:")) {
                    totalKb = parseLong(line.replaceAll("[^0-9]", ""));
                } else if (line.startsWith("SwapFree:")) {
                    freeKb = parseLong(line.replaceAll("[^0-9]", ""));
                }
            }
        } catch (Throwable e) {
            return new long[]{0, 0};
        }
        if (totalKb <= 0) {
            return new long[]{0, 0};
        }
        long usedKb = Math.max(0L, totalKb - freeKb);
        return new long[]{usedKb / 1024, totalKb / 1024};
    }

    private static long parseLong(String s) {
        try {
            return Long.parseLong(s.trim());
        } catch (Throwable e) {
            return 0;
        }
    }
}
