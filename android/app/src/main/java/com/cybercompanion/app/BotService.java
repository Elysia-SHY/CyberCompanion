package com.cybercompanion.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import androidx.core.app.NotificationCompat;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;

public class BotService extends Service {
    private static final String TAG = "CyberCompanionService";
    private static final String CHANNEL_ID = "cybercompanion_channel";
    private static final int NOTIFICATION_ID = 1001;

    private static Process botProcess;
    private static boolean isRunning = false;
    private static String statusMessage = "初始化中...";

    public static boolean isServiceRunning() {
        return isRunning;
    }

    public static String getStatusMessage() {
        return statusMessage;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        try {
            if (Build.VERSION.SDK_INT >= 34) {
                startForeground(NOTIFICATION_ID, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
            } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                startForeground(NOTIFICATION_ID, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
            } else {
                startForeground(NOTIFICATION_ID, buildNotification());
            }
        } catch (Throwable e) {
            Log.e(TAG, "startForeground with type failed: " + e.getMessage());
            try {
                startForeground(NOTIFICATION_ID, buildNotification());
            } catch (Throwable t) {
                Log.e(TAG, "Default startForeground failed: " + t.getMessage());
            }
        }

        if (!isRunning) {
            startGoEngine();
        }
        return START_STICKY;
    }

    private void startGoEngine() {
        isRunning = true;
        statusMessage = "正在准备核心程序...";

        new Thread(() -> {
            try {
                File filesDir = getFilesDir();
                File configFile = new File(filesDir, "config.json");

                // 1. Initialize default config if not present
                if (!configFile.exists()) {
                    writeDefaultConfig(configFile);
                }

                // 2. Find or extract binary
                File binaryFile = getOrExtractBinary();
                if (binaryFile == null || !binaryFile.exists()) {
                    statusMessage = "错误：找不到适用于本机的核心二进制文件";
                    Log.e(TAG, statusMessage);
                    isRunning = false;
                    return;
                }

                statusMessage = "正在启动服务进程...";
                Log.i(TAG, "Launching binary: " + binaryFile.getAbsolutePath() + " -config " + configFile.getAbsolutePath());

                ProcessBuilder pb = new ProcessBuilder(
                        binaryFile.getAbsolutePath(),
                        "-config", configFile.getAbsolutePath(),
                        "-port", "8088"
                );
                pb.directory(filesDir);
                pb.environment().put("HOME", filesDir.getAbsolutePath());
                pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
                pb.environment().put("PATH", System.getenv("PATH") + ":" + filesDir.getAbsolutePath());
                pb.redirectErrorStream(true);

                botProcess = pb.start();
                statusMessage = "核心服务已在后台运行 (端口: 8088)";

                try (BufferedReader reader = new BufferedReader(new InputStreamReader(botProcess.getInputStream()))) {
                    String line;
                    while ((line = reader.readLine()) != null) {
                        Log.d("CyberCompanionCore", line);
                    }
                } catch (Exception e) {
                    Log.w(TAG, "Process stream closed: " + e.getMessage());
                }

                int exitCode = botProcess.waitFor();
                statusMessage = "核心进程已退出 (代码: " + exitCode + ")";
                Log.w(TAG, statusMessage);
                isRunning = false;

            } catch (Exception e) {
                statusMessage = "启动异常: " + e.getMessage();
                Log.e(TAG, "Failed to run Go core: ", e);
                isRunning = false;
            }
        }).start();
    }

    private File getOrExtractBinary() {
        // First check nativeLibraryDir
        String nativeLibDir = getApplicationInfo().nativeLibraryDir;
        File nativeFile = new File(nativeLibDir, "libcybercompanion.so");
        if (nativeFile.exists()) {
            nativeFile.setExecutable(true, false);
            if (nativeFile.canExecute()) {
                return nativeFile;
            }
        }

        // Second: fallback to extracting from APK into app internal storage
        File binDir = new File(getFilesDir(), "bin");
        if (!binDir.exists()) {
            binDir.mkdirs();
        }
        File extractedFile = new File(binDir, "cybercompanion");
        if (extractFromApk(extractedFile)) {
            extractedFile.setExecutable(true, false);
            extractedFile.setReadable(true, false);
            try {
                Runtime.getRuntime().exec(new String[]{"chmod", "755", extractedFile.getAbsolutePath()}).waitFor();
            } catch (Exception ignored) {}
            return extractedFile;
        }

        return nativeFile.exists() ? nativeFile : null;
    }

    private boolean extractFromApk(File dest) {
        ZipFile zip = null;
        try {
            String apkPath = getApplicationInfo().sourceDir;
            zip = new ZipFile(apkPath);
            String[] preferredAbis = Build.SUPPORTED_ABIS;
            ZipEntry targetEntry = null;

            for (String abi : preferredAbis) {
                ZipEntry entry = zip.getEntry("lib/" + abi + "/libcybercompanion.so");
                if (entry != null) {
                    targetEntry = entry;
                    Log.i(TAG, "Matched ABI: " + abi);
                    break;
                }
            }

            if (targetEntry == null) {
                targetEntry = zip.getEntry("lib/arm64-v8a/libcybercompanion.so");
            }
            if (targetEntry == null) {
                targetEntry = zip.getEntry("lib/armeabi-v7a/libcybercompanion.so");
            }

            if (targetEntry == null) {
                Log.e(TAG, "No suitable library entry in APK");
                return false;
            }

            if (dest.exists() && dest.length() == targetEntry.getSize()) {
                return true;
            }

            try (InputStream is = zip.getInputStream(targetEntry);
                 FileOutputStream fos = new FileOutputStream(dest)) {
                byte[] buffer = new byte[16384];
                int len;
                while ((len = is.read(buffer)) > 0) {
                    fos.write(buffer, 0, len);
                }
                fos.flush();
            }
            Log.i(TAG, "Extracted " + targetEntry.getName() + " to " + dest.getAbsolutePath());
            return true;
        } catch (Exception e) {
            Log.e(TAG, "Extraction failed: ", e);
            return false;
        } finally {
            if (zip != null) {
                try { zip.close(); } catch (Exception ignored) {}
            }
        }
    }

    private void writeDefaultConfig(File dest) {
        String defaultConfig = "{\n" +
                "  \"qq_appid\": \"\",\n" +
                "  \"qq_secret\": \"\",\n" +
                "  \"oneapi_url\": \"https://api.deepseek.com/v1/chat/completions\",\n" +
                "  \"oneapi_token\": \"\",\n" +
                "  \"model\": \"deepseek-chat\",\n" +
                "  \"bot_name\": \"DEEPSEEK-CHAN\",\n" +
                "  \"system_prompt\": \"你是一个温柔贴心的二次元日常陪伴少女。\",\n" +
                "  \"active_persona\": \"deepseek_chan\",\n" +
                "  \"owners\": [],\n" +
                "  \"owners_file\": \"owners.json\",\n" +
                "  \"daily_file\": \"daily_traffic.json\",\n" +
                "  \"passcode\": \"复活吧我的爱人！！！elyisa\",\n" +
                "  \"web_port\": 8088,\n" +
                "  \"sandbox\": false,\n" +
                "  \"stickers_dir\": \"./stickers\",\n" +
                "  \"enable_stickers\": true\n" +
                "}";
        try (FileOutputStream fos = new FileOutputStream(dest)) {
            fos.write(defaultConfig.getBytes("UTF-8"));
            Log.i(TAG, "Default config created at: " + dest.getAbsolutePath());
        } catch (Exception e) {
            Log.e(TAG, "Error writing default config: ", e);
        }
    }

    private void createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel channel = new NotificationChannel(
                    CHANNEL_ID,
                    "CyberCompanion 伴侣前台服务",
                    NotificationManager.IMPORTANCE_LOW
            );
            channel.setDescription("保持 QQ 机器人与 Web 控制台 24 小时后台运行");
            NotificationManager manager = getSystemService(NotificationManager.class);
            if (manager != null) {
                manager.createNotificationChannel(channel);
            }
        }
    }

    private Notification buildNotification() {
        Intent notificationIntent = new Intent(this, MainActivity.class);
        PendingIntent pendingIntent = PendingIntent.getActivity(
                this, 0, notificationIntent,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT
        );

        return new NotificationCompat.Builder(this, CHANNEL_ID)
                .setContentTitle("CyberCompanion 运行中")
                .setContentText("QQ 机器人服务与本地 Web 控制台 (:8088) 在线")
                .setSmallIcon(R.drawable.ic_launcher)
                .setContentIntent(pendingIntent)
                .setOngoing(true)
                .build();
    }

    @Override
    public void onDestroy() {
        if (botProcess != null) {
            botProcess.destroy();
        }
        isRunning = false;
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
