from locustfile import HttpUser, task, between
import json
import random
import time
from datetime import datetime

class GoServiceUser(HttpUser):
    # Настройки для нагрузки 1000 RPS
    wait_time = between(0.001, 0.002)  
    
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.device_ids = [f"device_{i}" for i in range(1, 1001)]
        self.request_count = 0
    
    @task(60)  # 60% трафика - отправка метрик
    def submit_metrics(self):
        """Отправка метрик на /analyze эндпоинт"""
        payload = {
            "timestamp": datetime.utcnow().isoformat() + "Z",
            "cpu": round(random.uniform(10.0, 90.0), 2),
            "rps": round(random.uniform(50.0, 200.0), 2)
        }
        
        with self.client.post("/analyze", 
                             json=payload,
                             headers={"Content-Type": "application/json"},
                             catch_response=True,
                             name="POST_Analyze") as response:
            
            self.request_count += 1
            
            if response.status_code == 202:
                response.success()
                # Проверяем наличие аномалий в ответе
                try:
                    resp_data = response.json()
                    if 'status' in resp_data and resp_data['status'] == 'accepted':
                        # Успешная обработка
                        if self.request_count % 100 == 0:
                            print(f"✅ Successfully processed {self.request_count} requests")
                except:
                    pass
            else:
                response.failure(f"Status: {response.status_code}")
    
    @task(25)  # 25% трафика - получение метрик Prometheus
    def get_prometheus_metrics(self):
        """Получение метрик Prometheus"""
        with self.client.get("/metrics", 
                           catch_response=True,
                           name="GET_Metrics") as response:
            if response.status_code == 200:
                response.success()
                # Мониторинг нагрузки в реальном времени
                if "go_service_requests_total" in response.text:                    
                    pass
            else:
                response.failure(f"Metrics failed: {response.status_code}")
    
    @task(10)  # 10% трафика - health checks
    def health_check(self):
        """Проверка здоровья сервиса"""
        endpoints = [
            ("/health", "HealthCheck"),
            ("/count", "RequestCount"),
            ("/", "Root")
        ]
        
        for endpoint, name in endpoints:
            with self.client.get(endpoint, 
                               catch_response=True,
                               name=f"GET_{name}") as response:
                if response.status_code in [200, 202]:
                    response.success()
                else:
                    response.failure(f"{endpoint}: {response.status_code}")
    
    def on_start(self):
        """Вызывается при старте каждого виртуального пользователя"""
        print(f"🚀 Virtual user started - targeting {self.host}")

class AnomalyDetectionUser(HttpUser):
    """Специальный пользователь для тестирования обнаружения аномалий"""
    wait_time = between(0.1, 0.5)  
    
    @task(10)
    def submit_normal_metrics(self):
        """Нормальные метрики"""
        payload = {
            "timestamp": datetime.utcnow().isoformat() + "Z",
            "cpu": round(random.uniform(20.0, 60.0), 2),
            "rps": round(random.uniform(80.0, 120.0), 2)
        }
        self.client.post("/analyze", json=payload)
    
    @task(1)  
    def submit_anomaly(self):
        """Метрики с аномалиями для тестирования детектора"""
        payload = {
            "timestamp": datetime.utcnow().isoformat() + "Z",
            "cpu": round(random.uniform(85.0, 95.0), 2),  # Высокая нагрузка
            "rps": round(random.uniform(300.0, 500.0), 2)  # Аномальный RPS
        }
        
        with self.client.post("/analyze", 
                             json=payload,
                             catch_response=True,
                             name="POST_Anomaly") as response:
            if response.status_code == 202:
                print("🚨 Anomaly metric submitted for detection")
                response.success()
