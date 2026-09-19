package com.cybercompanion.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import androidx.core.app.NotificationCompat;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;

public class BotService extends Service {
    private static final String TAG = "CyberCompanionService";
    private static final String CHANNEL_ID = "cybercompanion_channel";
    private static final int NOTIFICATION_ID = 1001;

    private Process botProcess;
    private Thread processLogThread;
    private boolean isRunning = false;

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        startForeground(NOTIFICATION_ID, buildNotification());
        if (!isRunning) {
            startGoEngine();
        }
        return START_STICKY;
    }

    private void startGoEngine() {
        isRunning = true;
        new Thread(() -> {
            try {
                String nativeLibDir = getApplicationInfo().nativeLibraryDir;
                File binaryFile = new File(nativeLibDir, "libcybercompanion.so");

                if (!binaryFile.exists() || !binaryFile.canExecute()) {
                    Log.e(TAG, "Native binary missing or not executable at: " + binaryFile.getAbsolutePath());
                    return;
                }

                File filesDir = getFilesDir();
                File configFile = new File(filesDir, "config.json");

                // Initialize default config if not present
                if (!configFile.exists()) {
                    writeDefaultConfig(configFile);
                }

                Log.i(TAG, "Launching binary: " + binaryFile.getAbsolutePath() + " -config " + configFile.getAbsolutePath());

                ProcessBuilder pb = new ProcessBuilder(
                        binaryFile.getAbsolutePath(),
                        "-config", configFile.getAbsolutePath(),
                        "-port", "8088"
                );
                pb.directory(filesDir);
                pb.redirectErrorStream(true);

                botProcess = pb.start();

                processLogThread = new Thread(() -> {
                    try (BufferedReader reader = new BufferedReader(new InputStreamReader(botProcess.getInputStream()))) {
                        String line;
                        while ((line = reader.readLine()) != null) {
                            Log.d("CyberCompanionCore", line);
                        }
                    } catch (Exception e) {
                        Log.w(TAG, "Process reader closed: " + e.getMessage());
                    }
                });
                processLogThread.start();

                int exitCode = botProcess.waitFor();
                Log.w(TAG, "Core process exited with code: " + exitCode);
                isRunning = false;

            } catch (Exception e) {
                Log.e(TAG, "Failed to run Go core: ", e);
                isRunning = false;
            }
        }).start();
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
